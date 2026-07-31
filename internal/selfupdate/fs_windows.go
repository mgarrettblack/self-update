//go:build windows

package selfupdate

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// lockRegionLength is how many bytes of the lock file the byte-range lock
// covers. Windows has no whole-file lock primitive, so the convention is to
// lock a fixed region; one byte at offset 0 is enough because every instance
// agrees to lock exactly that region. The region need not actually exist in the
// file — locking past end-of-file is legal — which is why the lock file can
// stay empty.
const lockRegionLength = 1

// lockFile takes an exclusive byte-range lock on f, failing immediately rather
// than queueing, so a contended update is skipped instead of stalling startup.
func lockFile(f *os.File) error {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockRegionLength,
		0,
		&overlapped,
	)
	if err == nil {
		return nil
	}
	// With LOCKFILE_FAIL_IMMEDIATELY, contention surfaces as
	// ERROR_LOCK_VIOLATION. ERROR_IO_PENDING should not occur for a
	// non-overlapped handle, but it is treated as contention too: if the call
	// did somehow queue, this instance does not hold the lock and must not act
	// as though it does.
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLocked
	}
	return err
}

// unlockFile releases the byte-range lock. The region must match the one passed
// to LockFileEx exactly.
func unlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		lockRegionLength,
		0,
		&overlapped,
	)
}

// freeSpace reports the bytes this process may still write to the volume
// containing dir.
//
// GetDiskFreeSpaceEx returns three numbers; this uses freeBytesAvailableToCaller
// rather than totalNumberOfFreeBytes because the former honours per-user disk
// quotas. On a quota-managed machine the volume can have gigabytes free while
// this user is allowed none of it, and a preflight that ignored the quota would
// wave the download through into a mid-write failure.
func freeSpace(dir string) (uint64, error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}

	var availableToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &availableToCaller, &totalBytes, &totalFree); err != nil {
		return 0, err
	}
	return availableToCaller, nil
}

// brokenSuffix holds the executable being displaced during a rollback. On
// Windows the file cannot be deleted while the process running it is alive, so
// it is renamed aside and cleaned up later.
const brokenSuffix = ".broken"

// applySwap performs the Windows rename dance.
//
// Windows will not let anything overwrite the bytes of a running executable —
// the image is mapped and the file is held open without FILE_SHARE_WRITE — so
// the unix "rename the new file over the old path" trick does not work. What the
// loader *does* permit is renaming the running .exe, because it opens the file
// with FILE_SHARE_DELETE. So:
//
//  1. remove any stale app.exe.old left by an earlier update,
//  2. rename app.exe    -> app.exe.old   (moves the running image out of the way),
//  3. rename app.new.exe -> app.exe      (installs the new one).
//
// Unlike unix this is not a single atomic step: between (2) and (3) there is no
// file at the target path. If (3) fails, .old is renamed straight back so the
// app is never left without an executable where its shortcuts, services, and
// scheduled tasks expect one — a failed update that strands the install is far
// worse than a failed update that changes nothing.
//
// app.exe.old is deliberately not deleted here: the outgoing process is still
// executing it. RemoveOld on the next successful launch does that cleanup.
func applySwap(newBinary, target string) error {
	old := OldPath(target)

	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return swapError("apply: remove stale old", err)
	}

	targetExists := true
	if _, err := os.Lstat(target); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return swapError("apply: stat target", err)
		}
		// First install: there is nothing to move aside or retain.
		targetExists = false
	}

	if targetExists {
		if err := os.Rename(target, old); err != nil {
			return swapError("apply: rename target aside", err)
		}
	}

	if err := os.Rename(newBinary, target); err != nil {
		if targetExists {
			// Best effort: put the working executable back. If this also fails
			// there is nothing further we can do from here, but the original
			// binary is still on disk at .old for a human (or the next run of
			// the installer) to recover.
			if restoreErr := os.Rename(old, target); restoreErr != nil {
				return classifyf(ClassSwapFailed, "apply",
					"rename staged binary failed (%v) and restoring %q failed too (%v)",
					err, target, restoreErr)
			}
		}
		return swapError("apply: rename staged binary", err)
	}
	return nil
}

// restoreOld puts the retained generation back at target.
//
// The rollback runs from inside the *new* (bad) binary, which is therefore the
// running image at target and cannot be overwritten or deleted. So it is
// renamed aside to target+".broken" first, mirroring applySwap. The aside file
// is removed best-effort; while the current process is still executing it the
// delete fails, and it gets cleaned up by the next update's stale-file removal
// or a later RestoreOld.
func restoreOld(old, target string) error {
	broken := target + brokenSuffix

	if err := os.Remove(broken); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A leftover .broken we cannot clear is not fatal on its own; only give
		// up if it actually blocks the rename below.
		broken = ""
	}

	movedAside := false
	if _, err := os.Lstat(target); err == nil {
		if broken == "" {
			return classifyf(ClassSwapFailed, "restore old",
				"cannot move %q aside: a previous .broken file is still in place", target)
		}
		if err := os.Rename(target, broken); err != nil {
			return swapError("restore old: rename current aside", err)
		}
		movedAside = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return swapError("restore old: stat target", err)
	}

	if err := os.Rename(old, target); err != nil {
		if movedAside {
			if restoreErr := os.Rename(broken, target); restoreErr != nil {
				return classifyf(ClassSwapFailed, "restore old",
					"rename retained generation failed (%v) and putting %q back failed too (%v)",
					err, target, restoreErr)
			}
		}
		return swapError("restore old: rename retained generation", err)
	}

	if movedAside {
		// Expected to fail while this process is still running the image it
		// points at; cleaned up on a later pass.
		_ = os.Remove(broken)
	}
	return nil
}

// RelaunchReplacesProcess reports whether Relaunch replaces the current
// process image (unix) or spawns a new process the caller must exit after
// (windows). Callers need to know whether Relaunch returns on success.
//
// On Windows it is false: there is no execve equivalent, so Relaunch starts a
// child and returns. The caller MUST then shut down and exit — the old process
// keeps app.exe.old open, and the new one only gets a clean directory once the
// old one is gone.
const RelaunchReplacesProcess = false

// createNewProcessGroup is CREATE_NEW_PROCESS_GROUP. The successor is put in its
// own group so that a console Ctrl+C or Ctrl+Break aimed at the outgoing
// process (or the shell that launched it) does not also kill the process we
// just started.
//
// DETACHED_PROCESS is deliberately *not* used: it would strip the successor of
// the console it inherited, which for a CLI means the user stops seeing output
// across an update. A new process group is enough to decouple lifetimes.
const createNewProcessGroup = 0x00000200

func init() { execProcess = execSpawn }

// execSpawn starts path as an independent child process and returns.
//
// Standard streams are inherited so the successor keeps whatever console or
// redirection the current process had. The child handle is released rather than
// waited on: the caller is about to exit, and the successor must not become a
// zombie of, or be tied to, a parent that is going away.
func execSpawn(path string, argv, env []string) error {
	// Built literally rather than via exec.Command: path is already absolute
	// and argv already carries the argv[0] the caller wants, so there is no PATH
	// lookup to do and no argv[0] for exec.Command to pick on our behalf.
	cmd := &exec.Cmd{
		Path:        path,
		Args:        argv,
		Env:         env,
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{CreationFlags: createNewProcessGroup},
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Release()
}
