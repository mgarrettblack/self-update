# Downloading the artifact

**When:** Editing `internal/selfupdate/net.go`, or debugging a resumed, truncated, corrupted or endlessly-retrying artifact download.

**Source of truth:** `internal/selfupdate/net.go`, `internal/selfupdate/constants.go` (the attempt, backoff, timeout and buffer-size tunables), `internal/selfupdate/update.go` (`Poller.apply`), `self-update-design.md` §1, §4. The code wins if this document disagrees with it.

---

`Downloader.Fetch(ctx, art, destPath)` downloads `art.URL` to `destPath`,
resuming a partial file if one is there, and verifies SHA-256 over the compressed
bytes before returning. **`destPath` contains the verified artifact only on a nil
return.**

`Poller.apply` passes `destPath = <target>.download`, in the target's own
directory — see [invariant 4](invariants.md). A nil `Poller.Downloader` means a
zero-value `&Downloader{}`, so every default below applies.

## Before the first request

`Fetch` lower-cases and trims `art.SHA256` (a hex digest differing only in case is
the same digest; rejecting it would be a pointless failure) and then calls
`art.validate()` — the *manifest's* own rules — reusing them so a direct caller
cannot bypass what `ParseManifest` would have enforced. A failure here is
`ClassManifestInvalid` and no request is made at all.

## `Accept-Encoding: identity`

```go
req.Header.Set("Accept-Encoding", "identity")
```

Left alone, Go's transport advertises gzip and transparently decompresses the
response. That breaks the digest in two ways: the bytes reaching the hash would
not be the bytes the release pipeline signed, and `Content-Length` would describe
neither. Setting the header explicitly also disables the transport's automatic
decoding, so a server that compresses anyway fails the digest check rather than
slipping through. The artifact is *already* zstd-compressed — transport-level
compression buys nothing and costs the verification chain.

## Single-pass hashing

`writeArtifactBody` streams the body into `destPath` in `downloadBufferSize`
(64 KiB) chunks, writing and hashing each chunk in the same pass. Verification
therefore costs no second read and the artifact is never held in memory — §4's
streaming-hash requirement.

Read and write failures are returned as *separate* values because they classify
differently: a truncated body is a resumable network problem, a failed write is a
full disk or a permissions problem (`ClassOf(writeErr)`), and retrying the latter
is pointless.

The file is `fsync`ed before close. A crash between the download and the swap must
not leave a file whose contents only ever existed in the page cache. Mode is
`0o600` (`privateFileMode`): a compressed blob owned by the updating user, not
something anything else should read or run.

## Resume

`resumeOffset(destPath, art.Size)` decides what may be reused:

| On-disk state | Result |
|---|---|
| missing, or not a regular file | offset 0 |
| size 0 | offset 0 |
| size >= `art.Size` | file removed, offset 0 |
| otherwise | offset = file size |

A file at or beyond the advertised size cannot be a proper prefix of this
artifact — it is left over from a different release, or is not the artifact at all
— so it is discarded rather than range-requested past the end.

With `offset > 0` the request carries `Range: bytes=<offset>-`.

**A hash cannot be resumed.** `seedHashFromPrefix` re-reads the first `offset`
bytes off disk to rebuild the SHA-256 state. If that read fails, the prefix is
unusable and the download restarts from zero (`offset, remaining, h = 0,
art.Size, sha256.New()`).

## Response handling

| Status | Behaviour |
|---|---|
| `200 OK` | The server ignored `Range` and is sending the whole artifact. `offset = 0` — appending would concatenate the prefix with a full copy and silently corrupt the file. |
| `206 Partial Content` | Append **only** if `Content-Range`'s first byte position equals `offset` exactly. Missing or disagreeing header → `offset = 0`, full restart. Never guessed at. |
| `416 Range Not Satisfiable` | Delete `destPath`. If `offset == 0` this is fatal (repeating an unranged request gets the same answer); otherwise retryable, with the partial discarded. |
| anything else | `retryableHTTPStatus`: 408, 429 and 5xx retry; other 4xx do not. |

The 200 case is the one worth remembering when reasoning about a server that
ignores `Range` — a plain static-file host that does not implement ranges still
works, it just never resumes.

## Length and digest checks

Before writing, if `resp.ContentLength >= 0` and it disagrees with
`art.Size - offset`, the download fails as `ClassHashMismatch`, `destPath` is
deleted, and it is **not** retried. The length the server commits to contradicts
the signed manifest — a content disagreement, not a transient blip, and whatever
is on disk is not a prefix of the real artifact.

Reads are capped at the advertised remaining length
(`io.LimitReader(resp.Body, remaining)`). Without the cap a malicious or broken
server can stream forever and fill the user's disk while claiming to deliver a few
megabytes. Overshoot is therefore impossible; a short body is detected as
`total != art.Size` and is resumable.

The final digest comparison is `strings.EqualFold(sum, art.SHA256)`. On mismatch
`destPath` is deleted and the error is `ClassHashMismatch`, never retried:

> a partial that disagrees with the manifest is not a prefix worth resuming, and
> leaving it in place would poison every subsequent attempt forever.

`ClassHashMismatch` is an `IsTamperSignal()` class and escalates to
`SeverityAlert` in telemetry — see
[error classes](../observability/error-classes.md).

## Retries and backoff

`defaultFetchAttempts = 4`. Update checks repeat hourly, so there is no value in
grinding here — a transient outage is better handled by the next cycle.
`defaultFetchTimeout = 15 * time.Minute` bounds one attempt end to end, generous
because an artifact can be tens of megabytes on a metered link and being cut off
is cheap: the next attempt resumes.

`backoffDelay` doubles from `BaseBackoff` (default 1s), caps at
`maxBackoffDelay = 30s`, and applies **equal jitter** — the returned delay lands
in `[term/2, term]`. Half the delay stays fixed so the schedule still demonstrably
grows per attempt. The jitter is not cosmetic: a CDN blip fails every client in
the fleet at the same instant, and without jitter they all retry in lockstep and
the synchronised herd keeps the origin down.

The `*rand.Rand` is per-call and freshly seeded, so two processes on the same
machine do not draw the same delays. A nil `rnd` means no jitter (the
deterministic lower bound), which keeps the function usable from any caller with
no source to hand.

`ctx.Err()` is checked *before* sleeping, so a cancelled context ends the call now
rather than after the rest of the schedule.

## Progress

`progressGate` drops any report that would move the count backwards. A restart
from zero genuinely rewinds the byte count, but a progress bar that jumps backwards
reads as a bug, so callers only ever see the high-water mark advance. A nil
`Progress` func is safe.

## Related

- [decompress](decompress.md) — the next step, and why it runs only after this one
- [disk space](disk-space.md) — the preflight that runs before any of this
- [verification order](../security/verification-order.md)
- [HTTP endpoints](../release-contract/http-endpoints.md)
