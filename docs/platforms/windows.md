# Windows Platform Layer

**When:** Editing `fs_windows.go`, or explaining why the Windows swap is not a single rename and why the caller must exit after a Windows relaunch.

**Source of truth:** `internal/selfupdate/fs_windows.go`, the neutral wrappers in `internal/selfupdate/fs.go`, and `ErrRestartRequired` in `internal/selfupdate/update.go`. §2 of `self-update-design.md`. The code wins if they disagree.

---

Everything Windows-specific is in one file, `fs_windows.go`, tagged `//go:build windows`.
The neutral wrappers that call into it (`Apply`, `AcquireLock`, `Relaunch`,
`ensureFreeSpace`) are in `fs.go`. Its unix counterpart is [unix.md](unix.md); the pairing
convention is in [build-tags.md](build-tags.md).

This file used to be four files (`apply_windows.go`, `lock_windows.go`,
`relaunch_windows.go`, `space_windows.go`); those names are gone, and the four behaviours
are organized below by behaviour instead.

**Read [known-gaps.md](known-gaps.md) before trusting any of what follows.** This file
compiles under `GOOS=windows` and has never been executed. Everything below describes
intent and reasoning, not observed behaviour.

## Why the swap cannot be a single rename

Windows will not let anything overwrite the bytes of a running executable: the image is
mapped and the file is held open without `FILE_SHARE_WRITE`. So the unix trick — rename
the new file directly over the live path — is simply unavailable. What the loader *does*
permit is **renaming** the running `.exe`, because it opens the file with
`FILE_SHARE_DELETE`. The entire swap is built out of that one permission, and the design
only claims the loader "typically" grants it (§2).

## The swap — the rename dance

`applySwap(newBinary, target)`:

1. `os.Remove(OldPath(target))` — clear any stale `app.exe.old` from an earlier update.
2. `os.Rename(target, old)` — move the running image out of the way. Skipped when
   `target` does not exist (first install; `targetExists` tracks this).
3. `os.Rename(newBinary, target)` — install the new one.

**Unlike unix this is not atomic: between (2) and (3) there is no file at the target
path.** That window is unavoidable, so the code is built to close it rather than to
pretend it isn't there. If (3) fails, `.old` is renamed straight back, because a failed
update that strands the install is far worse than a failed update that changes nothing —
shortcuts, services, and scheduled tasks all point at that path. If the put-back *also*
fails, `classifyf(ClassSwapFailed, ...)` reports both errors together; the original
binary is still on disk at `.old` for a human or a reinstaller to recover.

**`app.exe.old` is deliberately not deleted here** — the outgoing process is still
executing it. `RemoveOld`, called from `Poller.MarkHealthy` on the next successful
launch, does that cleanup. On unix `RemoveOld` is only about discarding the rollback
generation; on Windows it is also the rename dance's garbage collection, which is why the
relaunch and the swap cannot be reasoned about independently on this platform.

Note the asymmetry with unix: there is no hard-link retention step and no `chmod`.
Windows has no exec bit, and step (2) *is* the retention.

The two double-failure cases are the only places in either platform half that reach for
`classifyf` directly instead of routing through `swapError` — there is no neutral
equivalent for "both the operation and its undo failed." See
[../observability/error-classes.md](../observability/error-classes.md) and
[../update-cycle/apply-swap.md](../update-cycle/apply-swap.md).

## Rollback — `restoreOld` and the `.broken` fallback

Rollback runs from inside the **new (bad) binary**, which is therefore the running image
at `target` — it cannot be overwritten or deleted. So `restoreOld` mirrors `applySwap`
one level up:

1. Best-effort `os.Remove(target + brokenSuffix)` where `brokenSuffix` is `".broken"`.
   If that fails, `broken` is set to `""` — a leftover `.broken` is not fatal on its own,
   only if it actually blocks the rename below.
2. If `target` exists: rename it to `.broken` (`movedAside = true`). If `broken == ""`
   at this point there is nowhere to move it, and the function gives up with
   `ClassSwapFailed`.
3. `os.Rename(old, target)` — put the retained generation back. On failure, the aside
   file is renamed back; a double failure again reports both causes.
4. Best-effort `os.Remove(broken)`. **This is expected to fail** while the current
   process is still executing that image. It gets cleaned up by the next update's
   stale-file removal or a later `RestoreOld`.

`.broken` exists only because Windows cannot delete a running image. There is no unix
equivalent — `fs_unix.go`'s `restoreOld` is one rename.

Whether step 4's deferred cleanup reliably succeeds on a later pass is one of the
unverified assumptions listed in [known-gaps.md](known-gaps.md). See
[../update-cycle/rollback.md](../update-cycle/rollback.md) for the accounting above it.

## Locking — `LockFileEx`

Windows has no whole-file lock primitive, so the convention is a byte-range lock over a
fixed region: `lockRegionLength = 1` byte at offset 0. Every instance agrees to lock
exactly that region. Locking past end-of-file is legal, which is why the lock file can
stay empty.

Flags are `LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY` — fail rather than queue,
so a contended update is skipped instead of stalling startup. Contention surfaces as
`ERROR_LOCK_VIOLATION` and becomes `ErrLocked`, which `AcquireLock` in `fs.go` tags
`ClassLocked`. `ERROR_IO_PENDING` should not occur for a non-overlapped handle, but is
treated as contention too: if the call did somehow queue, this instance does not hold the
lock and must not act as though it does. `unlockFile` must pass the identical region.

A wrong region here would fail in the safe-looking direction — no lock actually held,
with no error — which is exactly why it is called out in
[known-gaps.md](known-gaps.md). See
[../update-cycle/locking.md](../update-cycle/locking.md).

## Free space — `GetDiskFreeSpaceEx`

Of the three numbers returned, `freeSpace` uses **`freeBytesAvailableToCaller`**, not
`totalNumberOfFreeBytes`, because only the former honours per-user disk quotas. On a
quota-managed machine the volume can have gigabytes free while this user is allowed none
of it, and a preflight that ignored the quota would wave the download straight through
into a mid-write failure.

As with the lock region, picking the wrong field of the three would produce an
over-optimistic number and no error at all. See
[../update-cycle/disk-space.md](../update-cycle/disk-space.md).

## Relaunch — spawn and exit

`RelaunchReplacesProcess` is the `const false` here. There is no `execve` equivalent, so
`execSpawn` — registered as `execProcess` by this file's `init()` — starts a child and
returns.

**This is why `ErrRestartRequired` exists.** `Poller.UpdateOnce` sets
`UpdateResult.RestartPending` when the relaunch returns nil, and `Poller.Run` turns that
into `ErrRestartRequired` — "update applied: this process must exit so its successor can
take over." The caller MUST shut down and exit: the old process keeps `app.exe.old` open,
and the new one only gets a clean directory once the old one is gone. `cmd/app/main.go`
handles this sentinel explicitly (see
[../architecture/cmd-app.md](../architecture/cmd-app.md)). On unix it is never returned
from a successful update, because the relaunch there does not return at all.

`execSpawn` builds an `exec.Cmd` literally rather than via `exec.Command`: `path` is
already absolute and `argv` already carries the `argv[0]` the caller wants, so there is
no PATH lookup to do and no `argv[0]` for `exec.Command` to pick on our behalf. Standard
streams are inherited so the successor keeps whatever console or redirection the current
process had, and `cmd.Process.Release()` drops the handle rather than waiting — the
parent is about to exit and must not tie the successor to it.

`CreationFlags` is `CREATE_NEW_PROCESS_GROUP` (`0x00000200`) so a console Ctrl+C or
Ctrl+Break aimed at the outgoing process, or the shell that launched it, does not also
kill the process we just started. **`DETACHED_PROCESS` is deliberately not used**: it
would strip the successor of the inherited console, which for a CLI means the user stops
seeing output across an update. A new process group is enough to decouple lifetimes.

See [../update-cycle/relaunch.md](../update-cycle/relaunch.md) and
[../update-cycle/lifecycle-ordering.md](../update-cycle/lifecycle-ordering.md) for the
ordering this sits inside.

---

Because nothing here has ever run, the cheapest thing you can do after editing this file
is `GOOS=windows go build ./...`. It proves the file compiles and nothing more — but on a
file no test and no CI ever touches, it is the only automated signal that exists.
