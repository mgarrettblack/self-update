package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is plain data: the three things the library cannot infer for itself.
// Every other tunable lives in constants.go, and there are no methods — the
// lifecycle is a set of package-level functions and the caller sequences them.
type Config struct {
	// ManifestURL is the full URL of the signed release manifest. HTTPS is
	// required unless SELFUPDATE_ALLOW_HTTP is set; see allowHTTP.
	ManifestURL string

	// TargetPath is the executable to replace: absolute, symlinks resolved.
	// Set by NewConfig and never re-derived, so every stage of one cycle acts
	// on the same path even if the process later chdirs or the symlink moves.
	TargetPath string

	// StateDir holds the update lock and the crash-loop marker. It must be
	// writable even when the install directory is not.
	StateDir string
}

// NewConfig resolves the target executable once, at startup, and rejects the
// two settings that have no sensible default. An empty targetPath means the
// running executable.
//
// Resolution happens here rather than per cycle because it is the same answer
// every time, and because a caller that gets an error deserves it before the
// first poll rather than on every one.
func NewConfig(manifestURL, targetPath, stateDir string) (Config, error) {
	const op = "configure updater"

	if strings.TrimSpace(manifestURL) == "" {
		return Config{}, fmt.Errorf("%s: no manifest URL configured", op)
	}
	if strings.TrimSpace(stateDir) == "" {
		return Config{}, fmt.Errorf("%s: no state directory configured", op)
	}

	path := targetPath
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", op, err)
		}
		path = exe
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", op, err)
	}
	target := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		// Replacing the symlink itself would leave the real binary stale, so
		// the swap has to act on what the symlink points at.
		target = resolved
	}
	// A failure here is not fatal: a target that does not exist yet is a first
	// install, and the absolute path is where it is going to land.

	return Config{
		ManifestURL: manifestURL,
		TargetPath:  target,
		StateDir:    stateDir,
	}, nil
}

// Decision is the outcome of a check. Manifest is populated whenever a manifest
// was fetched and verified, even when no update applies, so the caller can log
// what the published version is.
type Decision struct {
	Manifest        *Manifest
	Artifact        PlatformArtifact
	UpdateAvailable bool
	CurrentVersion  string
}

var checkClient = &http.Client{Timeout: defaultCheckTimeout}

func CheckForUpdate(ctx context.Context, cfg Config) (bool, error) {
	const op = "check for update"

	// Step 1: Validate the manifest URL is configured.
	if strings.TrimSpace(cfg.ManifestURL) == "" {
		return false, fmt.Errorf("%s: manifest URL is empty", op)
	}

	// Step 2: Fetch and parse the manifest from the URL.
	manifest, err := fetchManifest(ctx, checkClient, cfg.ManifestURL, defaultMaxManifestBytes)
	if err != nil {
		return false, err
	}

	// Step 3: Check if the published version is newer than the running version.
	return IsNewer(manifest.Version, CurrentVersion)
}

	// Step 4: Get the platform-specific artifact for this system.
	platform := PlatformKey()
	art, err := manifest.Artifact(platform)
	if err != nil {
		return d, nil
	}

	// Step 5: Verify the artifact URL uses HTTPS (unless explicitly allowed).
	if err := requireHTTPS(art.URL, "artifact"); err != nil {
		return nil, err
	}

	// Step 6: Mark the update as available.
	d.Artifact = art
	d.UpdateAvailable = true
	return d, nil
}

// ApplyUpdate runs the half of the cycle that touches the filesystem:
// preflight, download, decompress, swap, mark. d must come from
// CheckForUpdate with UpdateAvailable set — nothing here re-checks it.
//
// This stays one function on purpose. The order below is what makes a failure
// at any point leave the live binary exactly as it was, and it is not safe for
// a caller to interleave: marking pending before the swap fires a revert for
// an update that never happened, and marking after the relaunch never runs at
// all, because on unix Relaunch replaces the process image. With no test suite
// this function body is the only thing enforcing that.
//
// Every failure path runs the same staging cleanup.
func ApplyUpdate(ctx context.Context, cfg Config, d *Decision) error {
	target := cfg.TargetPath
	dir := filepath.Dir(target)
	compressed := target + DownloadSuffix
	staged := target + StagedSuffix

	// The lock covers download through swap. Two instances staging to the same
	// paths would interleave writes into one file and produce a binary that
	// matches no digest at all.
	if err := os.MkdirAll(cfg.StateDir, stateDirMode); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	// Step 1: acquire the update lock — covers download through swap.
	lock, err := AcquireLock(filepath.Join(cfg.StateDir, lockFilename))
	if err != nil {
		return err
	}

	defer func() {
		os.Remove(compressed)
		os.Remove(staged)
		lock.Release()
	}()

	// Step 2: preflight disk space — ensureFreeSpace checks the artifact and
	// its decompressed expansion, on the volume that will hold both. Checked
	// before the first byte is requested.
	need := d.Artifact.Size * (1 + decompressionRatioEstimate)
	if need < d.Artifact.Size { // overflow on an absurd declared size
		return fmt.Errorf("update: declared artifact size %d bytes is not plausible", d.Artifact.Size)
	}
	if err := ensureFreeSpace(dir, need); err != nil {
		return err
	}

	// Step 3: download — downloadArtifact verifies SHA-256 over the compressed
	// bytes, the ones the manifest describes, and returns nil only if they
	// match. The manifest itself is unauthenticated: signature verification is
	// not implemented, so this proves transfer integrity, not provenance.
	if err := downloadArtifact(ctx, d.Artifact, compressed); err != nil {
		return err
	}
	// Step 4: decompress — only now, on bytes that are already verified.
	if err := DecompressFile(compressed, staged); err != nil {
		return err
	}
	// Step 5: apply — swap the staged binary onto the target.
	if err := Apply(staged, target); err != nil {
		return err
	}

	// Step 6: mark pending — recorded after the swap, before relaunch.
	return markPending(cfg, d.CurrentVersion, d.Manifest.Version)
}

type Marker struct {
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	AppliedAt   time.Time `json:"applied_at"`
	Attempts    int       `json:"attempts"`
}

// StartupResult is what CheckStartup observed.
type StartupResult struct {
	Reverted bool
	Marker   *Marker // the marker that was found, if any
}

func markerPath(cfg Config) string { return filepath.Join(cfg.StateDir, markerFilename) }

// markPending records that an update was applied but has not yet reported
// healthy. It is unexported because ApplyUpdate is its only legitimate caller:
// a marker written by anyone else claims an update that never happened, and
// the next start would revert on the strength of it.
func markPending(cfg Config, fromVersion, toVersion string) error {
	if cfg.StateDir == "" {
		return fmt.Errorf("mark pending: no state dir configured")
	}
	if err := os.MkdirAll(cfg.StateDir, stateDirMode); err != nil {
		return fmt.Errorf("mark pending: create state dir: %w", err)
	}

	m := Marker{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		AppliedAt:   time.Now(),
		Attempts:    0,
	}
	return writeMarker(cfg, m)
}

// CheckStartup runs once at process start, before any real startup work, and
// reports whether the previous update was successful: Reverted is set when a
// marker survived from an update that never reached MarkHealthy, either
// because it is unparseable or because it has been attempted past
// maxStartAttempts without ever reporting healthy.
//
// CheckStartup does not itself roll anything back — that is Rollback's job.
// The caller checks Reverted and calls Rollback when it is set.
func CheckStartup(cfg Config) (StartupResult, error) {
	if cfg.StateDir == "" {
		return StartupResult{}, fmt.Errorf("check startup: no state dir configured")
	}

	data, err := os.ReadFile(markerPath(cfg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StartupResult{}, nil
		}
		return StartupResult{}, fmt.Errorf("check startup: read marker: %w", err)
	}

	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		// Unparseable marker — there is no attempt count to trust, so treat
		// this as an update that never succeeded. Marker stays nil: nothing
		// was read that a caller could report.
		return StartupResult{Reverted: true}, nil
	}

	m.Attempts++
	if m.Attempts > maxStartAttempts {
		return StartupResult{Reverted: true, Marker: &m}, nil
	}

	if err := writeMarker(cfg, m); err != nil {
		// The attempt could not be recorded. Proceed rather than fail: an
		// unwritable state dir must not stop the app from starting.
		return StartupResult{Marker: &m}, err
	}
	return StartupResult{Marker: &m}, nil
}

// MarkHealthy clears the marker once startup has completed successfully. It is
// a no-op when there is no marker, so it is safe to call unconditionally at the
// end of startup on every run, not just post-update ones.
func MarkHealthy(cfg Config) error {
	if cfg.StateDir == "" {
		return fmt.Errorf("mark healthy: no state dir configured")
	}
	if err := removeFile(markerPath(cfg)); err != nil {
		return fmt.Errorf("mark healthy: %w", err)
	}
	return nil
}

// UpdateSuccessful clears the healthy marker and removes the retained .old
// binary now that startup has completed without error. It returns false if
// either step fails, signaling the caller should roll back rather than trust
// a swap that could not be confirmed.
func UpdateSuccessful(cfg Config) bool {
	if err := MarkHealthy(cfg); err != nil {
		return false
	}
	if err := RemoveOld(cfg.TargetPath); err != nil {
		return false
	}
	return true
}

// Rollback puts the previous generation back and clears the marker.
//
// Both errors are returned because no caller can otherwise observe them, and a
// silently failed restore is the most damaging state in the system: the process
// keeps running a binary that never reported healthy, with the marker already
// gone, so the next start will not retry.
func Rollback(cfg Config) {
	// Step 1: rename <TargetPath>.old back onto TargetPath.
	RestoreOld(cfg.TargetPath)

	// Step 2: remove the marker
	removeFile(markerPath(cfg))

}

// writeMarker persists m via a temp file plus rename, so a crash mid-write can
// never leave a partial marker at markerPath.
func writeMarker(cfg Config, m Marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("write marker: %w", err)
	}

	tmp := markerPath(cfg) + ".tmp"
	if err := os.WriteFile(tmp, data, privateFileMode); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	if err := os.Rename(tmp, markerPath(cfg)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write marker: rename: %w", err)
	}
	return nil
}

// removeFile deletes path, treating an already-absent file as success: every
// caller wants the file gone, and it already is.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
