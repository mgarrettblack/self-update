# Unix Platform Layer

**When:** Editing `fs_unix.go`, or explaining why the unix swap needs no dance, why the previous generation is retained by hard link, and why nothing runs after a successful relaunch.

**Source of truth:** `internal/selfupdate/fs_unix.go`, plus the neutral wrappers in `internal/selfupdate/fs.go`. §2 and §4 of `self-update-design.md`. The code wins if they disagree.

---

Everything unix-specific is in one file, `fs_unix.go`, tagged `//go:build !windows` —
linux and darwin share it. The neutral wrappers that call into it (`Apply`, `AcquireLock`,
`Relaunch`, `ensureFreeSpace`) are in `fs.go`, and all validation, path handling and error
classification happen there rather than here. Its Windows counterpart is
[windows.md](windows.md); the convention that pairs them is in
[build-tags.md](build-tags.md).

This file used to be four files (`apply_unix.go`, `lock_unix.go`, `relaunch_unix.go`,
`space_unix.go`). Those names are gone; only the four *behaviours* remain split, and they
are organized below by behaviour rather than by the files they once lived in.

## The swap — rename over a running executable

`applySwap(newBinary, target)` is three steps:

1. `os.Chmod(newBinary, binaryMode)` where `binaryMode` is `0o755`. The download lands as
   a plain `0o600`/`0o644` temp file, so the exec bits have to be added *before* the
   rename or the app replaces itself with something it cannot run.
2. `retainPrevious(target, OldPath(target))` — hard-link the current binary to
   `target + ".old"`.
3. `os.Rename(newBinary, target)`.

**Renaming over a running executable is safe on unix, and this is the reason the whole
unix path is simple.** A process does not execute "the file at this path"; it holds the
inode open via a file descriptor, and the kernel keeps that inode and its pages alive as
long as the descriptor exists. `rename(2)` only repoints the directory entry. So the
running process keeps executing the old image undisturbed while every new exec of that
path gets the new one. Windows cannot do this — contrast [windows.md](windows.md).

**The old generation is retained by hard link, not by renaming `target` aside.** A
rename-aside would open a window where the app's path does not exist at all. Linking
first means the `rename(2)` in step 3 is a single atomic step over a still-linked file,
and a crash at any instant leaves either the complete old binary or the complete new one
at `target` — never nothing. This is the concrete difference from Windows, which has no
choice but to accept that window.

`retainPrevious` handles three cases beyond the happy path:

- **First install** (`target` does not exist): nothing to retain. It also removes any
  stale `.old`, so a later rollback cannot resurrect a binary from an unrelated
  generation.
- **A pre-existing `.old`** is removed first, because `os.Link` fails with `EEXIST`.
- **Hard link refused**: some FUSE mounts, overlayfs upper layers, and network
  filesystems reject `os.Link`. Retention is what makes rollback possible, so it falls
  back to `copyFile` — slower and briefly twice the space, which the disk-space preflight
  already accounted for. `copyFile` syncs before close so the retained generation is
  durable rather than sitting in the page cache when the machine loses power mid-update.

`linkFile` is a package var (`= os.Link`) so that fallback is reachable without a
filesystem that refuses hard links. Its doc comment attributes this to tests; there are
none — see [known-gaps.md](known-gaps.md). The seam is still correct, and is the only way
that branch could ever be driven deliberately.

`restoreOld(old, target)` is a plain `os.Rename` plus a `chmod` back to `0o755`, atomic
and safe against the running executable for the same reason as `applySwap`. Contrast the
Windows version, which needs a `.broken` file to get the running image out of the way.

Neither function classifies its own errors: both route failures through `swapError` in
`fs.go`, which is why `fs_unix.go` contains no `ErrorClass` reference at all. See
[../observability/error-classes.md](../observability/error-classes.md) and
[../update-cycle/apply-swap.md](../update-cycle/apply-swap.md).

## Locking — `flock`

`lockFile` takes `unix.LOCK_EX|unix.LOCK_NB`. Non-blocking: a contended update is skipped
rather than stalling startup.

**flock state attaches to the open file description, not to the process.** A second open
of the same path — even from the same process — genuinely conflicts, which is what makes
`AcquireLock` usable as a single-instance guard with no process-level bookkeeping. It is
also why crashing without calling `Release` is safe: the OS drops the flock when the
handle is closed by process teardown, so a killed updater never leaves a lock that has to
be cleared by hand. That is the reason for a real file lock rather than a pidfile check.

Contention is reported as `EWOULDBLOCK`. Both `EWOULDBLOCK` and `EAGAIN` are checked
rather than switched on: they are the same value on Linux but not on some BSDs. Either
becomes `ErrLocked`, which `AcquireLock` in `fs.go` tags `ClassLocked` — callers use
`errors.Is(err, ErrLocked)` to tell "someone else is updating, fine, try next cycle" from
a real failure.

`unlockFile` is `LOCK_UN`. The lock file itself is never unlinked; `Lock.Release` in
`fs.go` explains why — unlinking would let two instances hold "the" lock on two different
inodes with the same name. See [../update-cycle/locking.md](../update-cycle/locking.md).

## Free space — `statfs`

`freeSpace` returns `Bavail * Bsize`.

**`Bavail`, not `Bfree`.** `Bfree` counts every unallocated block including the
percentage the filesystem reserves for root (5% by default on ext4, similar elsewhere).
An ordinary user process cannot use reserved blocks, so counting them would let the
preflight pass and the write still fail with `ENOSPC` — the exact outcome the check
exists to prevent. The explicit `uint64` conversions are there because the width and
signedness of `Bavail`/`Bsize` vary by platform.

The neutral `ensureFreeSpace` in `fs.go` adds `spaceSafetyMargin` (32 MiB) on top of the
requested size and returns `ClassDiskFull` on failure; the platform half only reports a
number. A stat failure is propagated rather than treated as "probably fine" — proceeding
on an unknown amount of space defeats the check.
See [../update-cycle/disk-space.md](../update-cycle/disk-space.md).

## Relaunch — `execve`, and it does not return

`RelaunchReplacesProcess` is the `const true` on unix. `execProcess` is set by this file's
`init()` to `execReplace`, which is `syscall.Exec(path, argv, env)`.

**It returns only on failure, so `Relaunch` returning nil is impossible and any code
after a successful `Relaunch` is dead.** Two consequences documented elsewhere: the
crash-loop marker must be written before the relaunch, and telemetry must be drained
before it — see [../update-cycle/rollback.md](../update-cycle/rollback.md),
[../update-cycle/invariants.md](../update-cycle/invariants.md) and
[../observability/telemetry-events.md](../observability/telemetry-events.md).

`execve` is preferred over fork+exec because it preserves the PID, the controlling
terminal, the open standard streams, and the parent's place in any process group or
supervisor's bookkeeping. Spawning a child and exiting the parent would make systemd or
launchd see the service as having exited, and would briefly leave two processes alive.

The neutral `Relaunch` in `fs.go` does everything around the exec: absolute-path
resolution, a stat preflight (worth a syscall precisely because a failure *after* the
point of no return is unrecoverable here), copying `argv` so `os.Args` is not mutated,
defaulting `argv[0]`, and classifying a permission failure as `ClassPermissionDenied`.

`ErrRestartRequired` is therefore not part of the unix success path: `Poller.UpdateOnce`
only sets `UpdateResult.RestartPending` when the relaunch *returns* nil, which the real
unix `Relaunch` cannot do. It is reachable on a unix host only through the
`Poller.Relaunch` seam. See [windows.md](windows.md), where it is the normal outcome, and
[../update-cycle/relaunch.md](../update-cycle/relaunch.md).

---

Cross-refs: [../update-cycle/apply-swap.md](../update-cycle/apply-swap.md),
[../update-cycle/locking.md](../update-cycle/locking.md),
[../update-cycle/disk-space.md](../update-cycle/disk-space.md) and
[../update-cycle/relaunch.md](../update-cycle/relaunch.md) for where these are called
from; [known-gaps.md](known-gaps.md) for what is not verified — which, on unix, is
everything except that these paths do execute during a manual run.
