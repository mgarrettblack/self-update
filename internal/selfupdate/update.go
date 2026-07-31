package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ErrLocked is returned when another instance already holds the update lock.
var ErrLocked = errors.New("another instance holds the update lock")

type Lock struct {
	path string

	mu   sync.Mutex
	file *os.File // nil once released
}

func AcquireLock(path string) (*Lock, error) {
	const op = "acquire lock"

	if path == "" {
		return nil, fmt.Errorf("%s: empty lock path", op)
	}

	// The state directory may not exist yet on a first run.
	if err := os.MkdirAll(filepath.Dir(path), lockDirMode); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// O_CREATE|O_RDWR, not O_TRUNC: truncating would mutate a file another
	// instance currently holds locked, and there is nothing in it to clear.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	return &Lock{path: path, file: f}, nil
}

func (l *Lock) Release() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	f := l.file
	l.file = nil
	l.mu.Unlock()

	if f == nil {
		return nil
	}

	unlockErr := unlockFile(f)
	closeErr := f.Close()

	err := errors.Join(unlockErr, closeErr)
	if err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}

func DecompressFile(src, dst string) error {
	const op = "decompress artifact"

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer in.Close()

	dec, err := zstd.NewReader(in)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer dec.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer out.Close()

	defer func() {
		if err != nil {
			os.Remove(dst)
		}
	}()

	written, copyErr := io.Copy(out, io.LimitReader(dec.IOReadCloser(), maxDecompressedBytes+1))
	closeErr := out.Close()

	switch {
	case copyErr != nil:
		err = fmt.Errorf("%s: %w", op, copyErr)
	case written > maxDecompressedBytes:
		err = fmt.Errorf("%s: artifact expands past the %d byte limit", op, maxDecompressedBytes)
	case closeErr != nil:
		err = fmt.Errorf("%s: %w", op, closeErr)
	case written == 0:
		err = fmt.Errorf("%s: artifact is empty", op)
	}
	return err
}

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

// FetchArtifact gets the platform-specific artifact from the manifest
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
		return fmt.Errorf("apply: resolve paths: %w", err)
	}
	if !sameDir {
		return fmt.Errorf("apply: staged binary %q is not in the target's directory %q: a cross-volume rename is not atomic",
			newBinary, filepath.Dir(target))
	}

	if _, err := os.Stat(newBinary); err != nil {
		return fmt.Errorf("apply: stat staged binary: %w", err)
	}

	return applySwap(newBinary, target)
}

func ApplyUpdate(ctx context.Context, cfg Config, manifest *Manifest) error {
	const op = "apply update"

	target := cfg.TargetPath
	compressed := target + DownloadSuffix
	staged := target + StagedSuffix

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

// markPending records that an update was applied but has not yet reported healthy
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

// UpdateSuccessful checks that previous install was successful
func UpdateSuccessful(cfg Config) bool {
	if cfg.StateDir == "" {
		return false
	}
	// Step 1: remove marker file
	if err := os.Remove(filepath.Join(cfg.StateDir, markerFilename)); err != nil {
		return false
	}
	// Step 2: remove old binary
	if err := os.Remove(OldPath(cfg.TargetPath)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return true
}

func Rollback(cfg Config) {
	// Step 1: rename <TargetPath>.old back onto TargetPath.
	os.Rename(cfg.TargetPath+oldSuffix, cfg.TargetPath)
	os.Chmod(cfg.TargetPath, binaryMode)

	// Step 2: remove the marker file
	os.Remove(filepath.Join(cfg.StateDir, markerFilename))

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
