package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// fetchBytes issues a GET against rawURL and returns its body, capped at max
// bytes. It is used for both the manifest and its detached signature.
func fetchBytes(ctx context.Context, client *http.Client, rawURL string, max int64) ([]byte, error) {
	op := "fetch " + urlPath(rawURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, classify(ClassInternal, op, err)
	}
	// Release buckets sit behind CDNs that will happily serve a cached manifest
	// long after a release has been pulled.
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return nil, classify(ClassNetwork, op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Includes 404: a missing signature file is a failure, never a licence
		// to treat the manifest as unsigned.
		return nil, classifyf(ClassNetwork, op, "unexpected status %s", resp.Status)
	}

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated — truncation would change the bytes the signature
	// covers and surface as a confusing signature failure.
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, classify(ClassNetwork, op, err)
	}
	if int64(len(data)) > max {
		return nil, classifyf(ClassManifestInvalid, op, "response exceeds the %d byte limit", max)
	}
	return data, nil
}

// requireHTTPS enforces transport security on a URL. The only waiver is
// SELFUPDATE_ALLOW_HTTP; see allowHTTP for why that is development-only.
func requireHTTPS(rawURL string, what string, class ErrorClass) error {
	op := "validate " + what + " URL"

	u, err := url.Parse(rawURL)
	if err != nil {
		return classifyf(class, op, "%q is not a valid URL: %v", rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if allowHTTP() {
		return nil
	}
	return classifyf(class, op, "%s URL uses scheme %q; HTTPS is required", what, u.Scheme)
}

// urlPath returns just the path component of a URL, for error messages. Full
// URLs in errors are fine locally but end up in shipped logs, so keep the
// host out of them.
func urlPath(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return u.Path
	}
	return "manifest"
}

// defaultDownloadClient is shared so that connection pooling and TLS session
// reuse survive across polls.
var defaultDownloadClient = &http.Client{Timeout: defaultFetchTimeout}

// downloadArtifact fetches art into destPath and verifies SHA-256 over the
// bytes written, retrying a resumable failure up to defaultFetchAttempts times.
// It returns nil only when the file on disk matches the digest the manifest
// advertises, which is the precondition DecompressFile and Apply rely on.
func downloadArtifact(ctx context.Context, art PlatformArtifact, destPath string) error {
	const op = "download artifact"

	art.SHA256 = strings.ToLower(strings.TrimSpace(art.SHA256))

	if err := art.validate(); err != nil {
		return classify(ClassManifestInvalid, op, err)
	}

	attempts := defaultFetchAttempts
	base := defaultBaseBackoff

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Checked before sleeping, so a cancelled context ends the call now
		// rather than after the rest of the schedule.
		if err := ctx.Err(); err != nil {
			return classify(ClassNetwork, op, err)
		}
		if attempt > 1 {
			delay := backoffDelay(attempt-1, base, rnd)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return classify(ClassNetwork, op, ctx.Err())
			case <-timer.C:
			}
		}

		retry, err := downloadAttempt(ctx, art, destPath)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return classifyf(ClassNetwork, op, "gave up after %d attempts: %w", attempts, lastErr)
}

// downloadAttempt is one pass of downloadArtifact's retry loop. retry reports
// whether the failure is worth another attempt; a nil error means destPath holds
// the full artifact and its digest matched.
func downloadAttempt(ctx context.Context, art PlatformArtifact, destPath string) (retry bool, err error) {
	const op = "download artifact"

	offset := resumeOffset(destPath, art.Size)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, art.URL, nil)
	if err != nil {
		return false, classify(ClassInternal, op, err)
	}

	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := defaultDownloadClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, classify(ClassNetwork, op, ctx.Err())
		}
		return true, classify(ClassNetwork, op, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0
	case http.StatusPartialContent:
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start != offset {
			offset = 0
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// This server will not serve our prefix; it is dead weight now.
		_ = os.Remove(destPath)
		if offset == 0 {
			// We did not ask for a range, so repeating the request would get
			// the same answer.
			return false, classifyf(ClassNetwork, op, "server rejected an unranged request with %s", resp.Status)
		}
		return true, classifyf(ClassNetwork, op,
			"server rejected resume at byte %d with %s; discarded the partial", offset, resp.Status)
	default:
		return retryableHTTPStatus(resp.StatusCode), classifyf(ClassNetwork, op, "unexpected status %s", resp.Status)
	}

	remaining := art.Size - offset
	if resp.ContentLength >= 0 && resp.ContentLength != remaining {
		_ = os.Remove(destPath)
		return false, classifyf(ClassHashMismatch, op,
			"server offers %d bytes from offset %d, manifest says %d", resp.ContentLength, offset, remaining)
	}

	h, err := seedHashFromPrefix(destPath, offset)
	if err != nil {
		offset, remaining, h = 0, art.Size, sha256.New()
	}

	written, readErr, writeErr := writeArtifactBody(destPath, offset, io.LimitReader(resp.Body, remaining), h)
	total := offset + written

	switch {
	case writeErr != nil:
		// Out of space or no permission to write here: neither improves on a
		// retry, and both need to be reported as themselves rather than as a
		// network blip.
		return false, classify(ClassOf(writeErr), op, writeErr)
	case readErr != nil:
		// Interrupted mid-stream. Keep the partial — resuming it is the whole
		// point of ranged requests.
		return true, classify(ClassNetwork, op, readErr)
	case total != art.Size:
		// The body ended cleanly but short (the cap above makes overshoot
		// impossible). Also resumable.
		return true, classifyf(ClassNetwork, op, "body ended after %d of %d bytes", total, art.Size)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(sum, art.SHA256) {
		_ = os.Remove(destPath)
		return false, classifyf(ClassHashMismatch, op, "sha256 %s does not match manifest %s", sum, art.SHA256)
	}
	return false, nil
}

// resumeOffset reports how many bytes of destPath may be reused as a prefix of
// the artifact, removing the file when they cannot be.
func resumeOffset(destPath string, size int64) int64 {
	info, err := os.Stat(destPath)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	if info.Size() <= 0 {
		return 0
	}
	// A file at or beyond the advertised size cannot be a proper prefix of the
	// artifact — it is left over from a different release, or is not the
	// artifact at all. Discard it instead of range-requesting past the end.
	if info.Size() >= size {
		_ = os.Remove(destPath)
		return 0
	}
	return info.Size()
}

// seedHashFromPrefix returns a SHA-256 state covering the first n bytes of path,
// which is the only way to carry a hash across a resumed download.
func seedHashFromPrefix(path string, n int64) (hash.Hash, error) {
	h := sha256.New()
	if n <= 0 {
		return h, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	copied, err := io.Copy(h, io.LimitReader(f, n))
	if err != nil {
		return nil, err
	}
	if copied != n {
		return nil, fmt.Errorf("re-hashed %d of %d partial bytes", copied, n)
	}
	return h, nil
}

// writeArtifactBody streams body into destPath while hashing it in the same
// pass, so verification costs no second read and the artifact is never held in
// memory.
//
// Read and write failures are returned separately because they classify
// differently: a truncated body is a resumable network problem, a failed write
// is a full disk or a permissions problem.
func writeArtifactBody(destPath string, offset int64, body io.Reader, h hash.Hash) (written int64, readErr, writeErr error) {
	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(destPath, flags, privateFileMode)
	if err != nil {
		return 0, nil, err
	}

	buf := make([]byte, downloadBufferSize)
	for {
		nr, rerr := body.Read(buf)
		if nr > 0 {
			nw, werr := f.Write(buf[:nr])
			if werr == nil && nw != nr {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				f.Close()
				return written, nil, werr
			}
			h.Write(buf[:nr]) // hash.Hash never errors
			written += int64(nr)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			f.Close()
			return written, rerr, nil
		}
	}

	// Flush to the device before the caller verifies and hands this path to the
	// swap step. A crash in between must not leave a file whose contents only
	// ever existed in the page cache.
	if err := f.Sync(); err != nil {
		f.Close()
		return written, nil, err
	}
	if err := f.Close(); err != nil {
		return written, nil, err
	}
	return written, nil, nil
}

// backoffDelay returns how long to wait before retry number attempt (1 is the
// first retry). The exponential term doubles per attempt from base and is capped
// at maxBackoffDelay; the returned delay is that term with "equal jitter"
// applied, so it always lands in [term/2, term] and is never negative.
//
// The jitter is not cosmetic. A CDN blip fails every client in the fleet at the
// same instant; without jitter they all retry in lockstep and the synchronised
// herd keeps the origin down. Half the delay is kept fixed so the schedule
// still demonstrably grows with each attempt.
//
// A nil rnd means no jitter (the deterministic lower bound), which keeps the
// function usable from tests and from any caller that has no source to hand.
func backoffDelay(attempt int, base time.Duration, rnd *rand.Rand) time.Duration {
	if attempt < 1 {
		return 0
	}
	if base <= 0 {
		base = defaultBaseBackoff
	}

	term := base
	for i := 1; i < attempt; i++ {
		// Stop before doubling can overflow into a negative duration.
		if term >= maxBackoffDelay/2 {
			term = maxBackoffDelay
			break
		}
		term *= 2
	}
	if term > maxBackoffDelay || term <= 0 {
		term = maxBackoffDelay
	}

	half := term / 2
	if rnd == nil || half <= 0 {
		return half
	}
	return half + time.Duration(rnd.Int63n(int64(half)+1))
}

// retryableHTTPStatus reports whether a status is worth another attempt. 408 and
// 429 are the two 4xx that mean "later, not never"; everything else in the 4xx
// range says the request itself is wrong, and repeating it just adds load.
func retryableHTTPStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}

// contentRangeStart parses the first byte position out of a Content-Range header
// such as "bytes 100-199/200". A response whose start cannot be read is treated
// as unusable for appending by the caller.
func contentRangeStart(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	const unit = "bytes "
	if len(v) < len(unit) || !strings.EqualFold(v[:len(unit)], unit) {
		return 0, false
	}
	spec := strings.TrimSpace(v[len(unit):])
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(spec[:dash]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
