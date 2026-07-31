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
	ManifestURL string
	TargetPath  string
	StateDir    string
}

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

func CheckForUpdate(ctx context.Context, cfg Config) (bool, *Manifest, error) {
	const op = "check for update"

	// Step 1: Validate the manifest URL is configured.
	if strings.TrimSpace(cfg.ManifestURL) == "" {
		return false, nil, fmt.Errorf("%s: manifest URL is empty", op)
	}

	// Step 2: Fetch and parse the manifest from the URL.
	manifest, err := fetchManifest(ctx, checkClient, cfg.ManifestURL, defaultMaxManifestBytes)
	if err != nil {
		return false, nil, err
	}

	// Step 3: Check if the published version is newer than the running version.
	available, err := IsNewer(manifest.Version, CurrentVersion)
	return available, manifest, err
}

// FetchArtifact gets the platform-specific artifact from the manifest, preflights
// disk space, downloads it, and verifies its SHA-256.
func FetchArtifact(ctx context.Context, manifest *Manifest, cfg Config) error {
	const op = "fetch artifact"

	target := cfg.TargetPath
	dir := filepath.Dir(target)
	compressed := target + DownloadSuffix

	// Step 1: get artifact metadata from manifest.
	platform := PlatformKey()
	art, err := manifest.Artifact(platform)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Step 2: preflight disk space check.
	need := art.Size
	if need < art.Size { // overflow on an absurd declared size
		return fmt.Errorf("%s: declared artifact size %d bytes is not plausible", op, art.Size)
	}
	if err := ensureFreeSpace(dir, need); err != nil {
		return err
	}

	// Step 3: download the artifact with SHA-256 verification.
	if err := downloadArtifact(ctx, art, compressed); err != nil {
		return err
	}

	return nil
}

func apply(newBinary, target string) error {
	if newBinary == "" || target == "" {
		return fmt.Errorf("apply: empty path (new=%q target=%q)", newBinary, target)
	}

	sameDir, err := sameDirectory(newBinary, target)
	if err != nil {
		return swapError("apply: resolve paths", err)
	}
	if !sameDir {
		return fmt.Errorf("apply: staged binary %q is not in the target's directory %q: a cross-volume rename is not atomic",
			newBinary, filepath.Dir(target))
	}

	if _, err := os.Stat(newBinary); err != nil {
		return swapError("apply: stat staged binary", err)
	}

	return applySwap(newBinary, target)
}

func ApplyUpdate(ctx context.Context, cfg Config, manifest *Manifest) error {
	const op = "apply update"

	target := cfg.TargetPath
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

	// Step 2: get the artifact for this platform, preflight disk space, and download.
	if err := FetchArtifact(ctx, manifest, cfg); err != nil {
		return err
	}

	// Step 3: decompress — only now, on bytes that are already verified.
	if err := DecompressFile(compressed, staged); err != nil {
		return err
	}

	// Step 4: apply — swap the staged binary onto the target.
	if err := apply(staged, target); err != nil {
		return err
	}

	// Step 5: mark pending — recorded after the swap, before relaunch.
	return markPending(cfg, CurrentVersion, manifest.Version)
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

// MarkHealthy clears the marker once startup has completed successfully. It is
// a no-op when there is no marker, so it is safe to call unconditionally at the
// end of startup on every run, not just post-update ones.
func MarkHealthy(cfg Config) error {
	if cfg.StateDir == "" {
		return fmt.Errorf("mark healthy: no state dir configured")
	}
	if err := removeFile(filepath.Join(cfg.StateDir, markerFilename)); err != nil {
		return fmt.Errorf("mark healthy: %w", err)
	}
	return nil
}

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
	removeFile(filepath.Join(cfg.StateDir, markerFilename))

}

// writeMarker persists m via a temp file plus rename, so a crash mid-write can
// never leave a partial marker at markerPath.
func writeMarker(cfg Config, m Marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("write marker: %w", err)
	}

	tmp := filepath.Join(cfg.StateDir, markerFilename) + ".tmp"
	if err := os.WriteFile(tmp, data, privateFileMode); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(cfg.StateDir, markerFilename)); err != nil {
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
