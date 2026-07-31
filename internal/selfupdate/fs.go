package selfupdate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
		return nil, classify(ClassInternal, op, errors.New("empty lock path"))
	}

	// The state directory may not exist yet on a first run.
	if err := os.MkdirAll(filepath.Dir(path), lockDirMode); err != nil {
		return nil, classify(ClassOf(err), op, err)
	}

	// O_CREATE|O_RDWR, not O_TRUNC: truncating would mutate a file another
	// instance currently holds locked, and there is nothing in it to clear.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFileMode)
	if err != nil {
		return nil, classify(ClassOf(err), op, err)
	}

	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, classify(ClassLocked, op, err)
		}
		return nil, classify(ClassOf(err), op, err)
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
		return classify(ClassOf(err), "release lock", err)
	}
	return nil
}

func ensureFreeSpace(dir string, needBytes int64) error {
	const op = "preflight free space"

	if needBytes <= 0 {
		return classifyf(ClassInternal, op, "invalid required size %d bytes", needBytes)
	}
	if dir == "" {
		return classify(ClassInternal, op, errors.New("empty directory path"))
	}

	avail, err := freeSpace(dir)
	if err != nil {
		return classify(ClassOf(err), op, err)
	}

	// needBytes is at most MaxInt64, so adding the margin cannot overflow
	// uint64; the assertion documents that rather than leaving it to the
	// reader.
	if uint64(needBytes) > math.MaxUint64-spaceSafetyMargin {
		return classifyf(ClassInternal, op, "required size %d bytes overflows", needBytes)
	}
	need := uint64(needBytes) + spaceSafetyMargin

	if avail < need {
		return classifyf(ClassDiskFull, op,
			"need %d bytes (%d artifact + %d margin), %d available",
			need, needBytes, uint64(spaceSafetyMargin), avail)
	}
	return nil
}

func OldPath(target string) string { return target + oldSuffix }

func Apply(newBinary, target string) error {
	if newBinary == "" || target == "" {
		return classifyf(ClassSwapFailed, "apply", "empty path (new=%q target=%q)", newBinary, target)
	}

	sameDir, err := sameDirectory(newBinary, target)
	if err != nil {
		return swapError("apply: resolve paths", err)
	}
	if !sameDir {
		return classifyf(ClassSwapFailed, "apply",
			"staged binary %q is not in the target's directory %q: a cross-volume rename is not atomic",
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
		return classifyf(ClassSwapFailed, "restore old", "empty target path")
	}
	old := OldPath(target)
	if _, err := os.Stat(old); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return classifyf(ClassSwapFailed, "restore old", "no retained generation at %q", old)
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
	if ClassOf(err) == ClassPermissionDenied {
		return classify(ClassPermissionDenied, op, err)
	}
	return classify(ClassSwapFailed, op, err)
}

var execProcess func(path string, argv, env []string) error

func Relaunch(path string, argv []string) error {
	if execProcess == nil {
		return classifyf(ClassInternal, "relaunch", "no exec primitive configured for this platform")
	}
	if path == "" {
		return classifyf(ClassInternal, "relaunch", "empty binary path")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return classify(ClassInternal, "relaunch: resolve path", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return classify(ClassPermissionDenied, "relaunch: stat", err)
		}
		return classify(ClassInternal, "relaunch: stat", err)
	}
	if info.IsDir() {
		return classifyf(ClassInternal, "relaunch", "%q is a directory", abs)
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
		if errors.Is(err, fs.ErrPermission) {
			return classify(ClassPermissionDenied, "relaunch", err)
		}
		return classify(ClassInternal, "relaunch", err)
	}
	return nil
}

func DecompressFile(src, dst string) error {
	const op = "decompress artifact"

	in, err := os.Open(src)
	if err != nil {
		return classify(ClassOf(err), op, err)
	}
	defer in.Close()

	dec, err := zstd.NewReader(in)
	if err != nil {
		return classify(ClassDecompression, op, err)
	}
	defer dec.Close()

	out, err := os.Create(dst)
	if err != nil {
		return classify(ClassOf(err), op, err)
	}

	// Read one byte past the limit so an artifact that exactly fills it is
	// still distinguishable from one that overruns.
	written, copyErr := io.Copy(out, io.LimitReader(dec.IOReadCloser(), maxDecompressedBytes+1))
	closeErr := out.Close()

	switch {
	case copyErr != nil:
		os.Remove(dst)
		return classify(ClassDecompression, op, copyErr)
	case written > maxDecompressedBytes:
		os.Remove(dst)
		return classifyf(ClassDecompression, op,
			"artifact expands past the %d byte limit", maxDecompressedBytes)
	case closeErr != nil:
		os.Remove(dst)
		return classify(ClassOf(closeErr), op, closeErr)
	case written == 0:
		os.Remove(dst)
		return classify(ClassDecompression, op, errors.New("artifact is empty"))
	}
	return nil
}

func InstallID(stateDir string) (string, error) {
	const op = "install id"

	if err := os.MkdirAll(stateDir, stateDirMode); err != nil {
		return "", classify(ClassOf(err), op, err)
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
		return "", classify(ClassOf(err), op, err)
	}

	buf := make([]byte, installIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", classify(ClassInternal, op, err)
	}
	id := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), privateFileMode); err != nil {
		return "", classify(ClassOf(err), op, err)
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
		return "", classify(ClassInternal, "state dir", err)
	}
	return filepath.Join(home, ".local", "state", app), nil
}

func localAppData() (string, error) {
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return dir, nil
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return "", classify(ClassInternal, "locate LOCALAPPDATA", err)
	}
	return dir, nil
}

func checkAppName(app string) error {
	if strings.TrimSpace(app) == "" {
		return classify(ClassInternal, "resolve directory", errors.New("app name is empty"))
	}
	return nil
}
