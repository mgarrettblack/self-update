//go:build !windows

package selfupdate

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking flock on f.
//
// flock state is attached to the *open file description*, not to the process,
// so a second open of the same path — even from this same process — genuinely
// conflicts. That is what makes AcquireLock usable as a single-instance guard
// without any extra process-level bookkeeping.
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return nil
	}
	// LOCK_NB reports contention as EWOULDBLOCK. On Linux EAGAIN and
	// EWOULDBLOCK are the same value; on some BSDs they are not, so both are
	// checked rather than switched on.
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrLocked
	}
	return err
}

// unlockFile drops the flock. The file is left in place; see Lock.Release for
// why it is not unlinked.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// freeSpace reports the bytes an unprivileged process may still write to the
// filesystem containing dir.
//
// It uses Bavail, not Bfree. Bfree counts every unallocated block, including
// the percentage the filesystem reserves for root (5% by default on ext4, and
// similar elsewhere). An ordinary user process cannot use reserved blocks, so
// counting them would let the preflight pass and the write still fail with
// ENOSPC — the exact outcome this check exists to prevent. Bavail is what is
// genuinely available to this process.
func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	// Field widths and signedness of Bavail/Bsize vary by platform, hence the
	// explicit conversions.
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// binaryMode is the permission set the swapped-in executable gets. The download
// lands as a plain 0o600/0o644 temp file, so the exec bits have to be added
// before the rename or the app replaces itself with something it cannot run.
const binaryMode fs.FileMode = 0o755

// linkFile is os.Link, indirected so the copy fallback in retainPrevious can be
// exercised in tests without needing a filesystem that refuses hard links.
var linkFile = os.Link

// applySwap performs the unix swap:
//
//  1. chmod the staged binary executable,
//  2. retain the current binary at target+".old" via a hard link,
//  3. rename(2) the staged binary over target.
//
// Renaming over a *running* executable is safe on unix. A process does not
// execute "the file at this path"; it holds the inode open via a file
// descriptor, and the kernel keeps that inode (and its pages) alive as long as
// the descriptor exists. rename(2) only repoints the directory entry, so the
// running process keeps executing the old image undisturbed while every new
// exec of that path gets the new one. This is why unix needs no rename dance —
// contrast the Windows applySwap below.
func applySwap(newBinary, target string) error {
	if err := os.Chmod(newBinary, binaryMode); err != nil {
		return swapError("apply: chmod staged binary", err)
	}
	if err := retainPrevious(target, OldPath(target)); err != nil {
		return err
	}
	if err := os.Rename(newBinary, target); err != nil {
		return swapError("apply: rename", err)
	}
	return nil
}

// retainPrevious links (or, failing that, copies) the current binary to old.
func retainPrevious(target, old string) error {
	if _, err := os.Lstat(target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing to retain — a first install rather than an update. Drop
			// any stale .old so a later rollback cannot resurrect a binary from
			// an unrelated generation.
			if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return swapError("apply: remove stale old", err)
			}
			return nil
		}
		return swapError("apply: stat target", err)
	}

	// os.Link fails with EEXIST if old is still there from the previous update.
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return swapError("apply: remove stale old", err)
	}

	if err := linkFile(target, old); err == nil {
		return nil
	}

	// The filesystem refused a hard link (some FUSE mounts, overlayfs upper
	// layers, and network filesystems do). Retention is what makes rollback
	// possible, so fall back to a copy: slower and briefly uses twice the
	// space, but the disk-space preflight already accounted for a second copy.
	if err := copyFile(target, old, binaryMode); err != nil {
		return swapError("apply: retain previous generation", err)
	}
	return nil
}

// restoreOld puts the retained generation back at target. On unix this is a
// plain rename, atomic and safe even though target is the running executable
// (same reasoning as applySwap).
func restoreOld(old, target string) error {
	if err := os.Rename(old, target); err != nil {
		return swapError("restore old: rename", err)
	}
	if err := os.Chmod(target, binaryMode); err != nil {
		return swapError("restore old: chmod", err)
	}
	return nil
}

// copyFile copies src to dst, syncing before close so the retained generation
// is durable rather than sitting in the page cache when the machine loses
// power mid-update.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// RelaunchReplacesProcess reports whether Relaunch replaces the current
// process image (unix) or spawns a new process the caller must exit after
// (windows). Callers need to know whether Relaunch returns on success.
//
// On unix it is true: Relaunch execs, so a nil return is impossible and any
// code after a successful Relaunch is dead.
const RelaunchReplacesProcess = true

func init() { execProcess = execReplace }

// execReplace replaces the running process image via execve(2).
//
// This is preferred over fork+exec on unix because it preserves the PID, the
// controlling terminal, open standard streams, and the parent's place in any
// process group or supervisor's bookkeeping. Spawning a child and exiting the
// parent instead would make an init system (systemd, launchd) see the service
// as having exited, and would briefly leave two processes alive.
//
// It returns only on failure.
func execReplace(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
