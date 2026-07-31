package selfupdate

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

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
