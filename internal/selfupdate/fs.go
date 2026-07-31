package selfupdate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

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

func ensureFreeSpace(dir string, needBytes int64) error {
	const op = "preflight free space"

	if needBytes <= 0 {
		return fmt.Errorf("%s: invalid required size %d bytes", op, needBytes)
	}
	if dir == "" {
		return fmt.Errorf("%s: empty directory path", op)
	}

	avail, err := freeSpace(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// needBytes is at most MaxInt64, so adding the margin cannot overflow
	// uint64; the assertion documents that rather than leaving it to the
	// reader.
	if uint64(needBytes) > math.MaxUint64-spaceSafetyMargin {
		return fmt.Errorf("%s: required size %d bytes overflows", op, needBytes)
	}
	need := uint64(needBytes) + spaceSafetyMargin

	if avail < need {
		return fmt.Errorf("%s: need %d bytes (%d artifact + %d margin), %d available",
			op, need, needBytes, uint64(spaceSafetyMargin), avail)
	}
	return nil
}

func OldPath(target string) string { return target + oldSuffix }

func Apply(newBinary, target string) error {
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

func RemoveOld(target string) error {
	if err := os.Remove(OldPath(target)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return swapError("remove old", err)
	}
	return nil
}

func RestoreOld(target string) error {
	if target == "" {
		return fmt.Errorf("restore old: empty target path")
	}
	old := OldPath(target)
	if _, err := os.Stat(old); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restore old: no retained generation at %q", old)
		}
		return swapError("restore old: stat", err)
	}
	return restoreOld(old, target)
}

func sameDirectory(a, b string) (bool, error) {
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}

	dirA, dirB := filepath.Dir(absA), filepath.Dir(absB)
	if dirA == dirB {
		return true, nil
	}

	realA, errA := filepath.EvalSymlinks(dirA)
	realB, errB := filepath.EvalSymlinks(dirB)
	if errA != nil || errB != nil {
		// One of the directories does not exist (or is unreadable). The
		// textual comparison already said they differ; treat that as final.
		return false, nil
	}
	return realA == realB, nil
}

func swapError(op string, err error) error {
	return fmt.Errorf("%s: %w", op, err)
}

var execProcess func(path string, argv, env []string) error

func Relaunch(path string, argv []string) error {
	if execProcess == nil {
		return fmt.Errorf("relaunch: no exec primitive configured for this platform")
	}
	if path == "" {
		return fmt.Errorf("relaunch: empty binary path")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("relaunch: resolve path: %w", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("relaunch: stat: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("relaunch: %q is a directory", abs)
	}

	// argv is copied before any normalisation: the caller usually passes
	// os.Args, and mutating that would be a surprising side effect.
	next := make([]string, len(argv))
	copy(next, argv)
	if len(next) == 0 {
		next = []string{abs}
	} else if next[0] == "" {
		next[0] = abs
	}

	if err := execProcess(abs, next, os.Environ()); err != nil {
		return fmt.Errorf("relaunch: %w", err)
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

	// Read one byte past the limit so an artifact that exactly fills it is
	// still distinguishable from one that overruns.
	written, copyErr := io.Copy(out, io.LimitReader(dec.IOReadCloser(), maxDecompressedBytes+1))
	closeErr := out.Close()

	switch {
	case copyErr != nil:
		os.Remove(dst)
		return fmt.Errorf("%s: %w", op, copyErr)
	case written > maxDecompressedBytes:
		os.Remove(dst)
		return fmt.Errorf("%s: artifact expands past the %d byte limit", op, maxDecompressedBytes)
	case closeErr != nil:
		os.Remove(dst)
		return fmt.Errorf("%s: %w", op, closeErr)
	case written == 0:
		os.Remove(dst)
		return fmt.Errorf("%s: artifact is empty", op)
	}
	return nil
}

func InstallID(stateDir string) (string, error) {
	const op = "install id"

	if err := os.MkdirAll(stateDir, stateDirMode); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	path := filepath.Join(stateDir, installIDFile)

	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); len(id) == installIDBytes*2 {
			if _, err := hex.DecodeString(id); err == nil {
				return id, nil
			}
		}
		// Truncated or hand-edited: replace it rather than propagate a value
		// that would scatter this client across cohorts unpredictably.
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	buf := make([]byte, installIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	id := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), privateFileMode); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	return id, nil
}

func DefaultStateDir(app string) (string, error) {
	if err := checkAppName(app); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		local, err := localAppData()
		if err != nil {
			return "", err
		}
		return filepath.Join(local, app, "state"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", app), nil
}

func localAppData() (string, error) {
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return dir, nil
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate LOCALAPPDATA: %w", err)
	}
	return dir, nil
}

func checkAppName(app string) error {
	if strings.TrimSpace(app) == "" {
		return fmt.Errorf("resolve directory: app name is empty")
	}
	return nil
}
