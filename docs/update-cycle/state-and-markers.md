# State directory and on-disk markers

**When:** Debugging an installation by inspecting its files, changing anything that writes to the state directory or alongside the target binary, or answering "what should be on disk right now".

**Source of truth:** `internal/selfupdate/fs.go` (`InstallID`, `DefaultStateDir`, `AcquireLock`, `Apply`, `DecompressFile`, `OldPath`), `internal/selfupdate/update.go` (`Guard`, `Poller.apply`, `Poller.target`), `internal/selfupdate/fs_windows.go` (`brokenSuffix`), `internal/selfupdate/constants.go` (the filenames, suffixes and modes below), `self-update-design.md` §3, §6. The code wins if this document disagrees with it.

---

There are **two** directories, and the split is deliberate.

## The state directory

`Poller.StateDir`, required. Defaults come from `DefaultStateDir(app)`:

| Platform | Path |
|---|---|
| unix | `$HOME/.local/state/<app>` |
| Windows | `%LOCALAPPDATA%\<app>\state` (falling back to `os.UserConfigDir()` if the env var is unset) |

Namespaced by app so two applications using this package never share a lock file.
Created on demand at `0o700` (`stateDirMode`) by `Poller.apply`, `Guard.MarkPending`
and `InstallID` — each of them `MkdirAll`s rather than assuming a previous run got
there first. The one exception is `AcquireLock`, which creates the *lock file's*
parent at `0o755` (`lockDirMode`), because a caller may have pointed `LockPath`
outside the state directory entirely; see [locking](locking.md).

`0o700` is the mode because nothing outside this user has any business reading —
or, worse, writing — files the updater trusts.

The state directory must be writable **even when the install directory is not**.
The binary's directory may be read-only for the user (§6 — locked-down corporate
machines), and the crash-loop marker in particular has to be writable or rollback
cannot work at all. That is why the marker lives here rather than next to the
binary.

### Contents

| File | Written by | Deleted by | Notes |
|---|---|---|---|
| `install-id` | `InstallID(stateDir)` on first use | never by this package | 32 hex chars + newline (`installIDBytes = 16`), mode `0o600` (`privateFileMode`). Random, not derived from hostname or username, because it is attached to every telemetry event. |
| `update.lock` | `AcquireLock` | **never** | Mode `0o644` (`lockFileMode`), always empty. Its presence means nothing — see [locking](locking.md). |
| `update-pending.json` | `Guard.MarkPending` (old process, after the swap) | `Guard.MarkHealthy`, and `Guard.revert` | Mode `0o600`. The crash-loop marker. Presence = an update was applied and never confirmed healthy. |
| `update-pending.json.tmp` | `Guard.writeMarker`, transiently | renamed away immediately; removed on a failed rename | A lingering one means a crash mid-write; harmless. |

`install-id` is replaced rather than trusted if it is truncated or hand-edited —
propagating a malformed value would scatter the client across cohorts
unpredictably. It must survive restarts: rollout cohorting keys on it, and an id
that changed every boot would let a 10% release reach the whole fleet by attrition.
Deleting it therefore re-rolls this client's cohort for every release; see
[rollout cohorts](rollout-cohorts.md).

## The install directory

`Poller.TargetPath`, or — when that is empty — `os.Executable()`, in both cases made
absolute and then symlink-resolved by `Poller.target`.

**The package chooses no install location for you.** There is no
install-directory helper: `DefaultStateDir` covers state only, and where the binary
itself lives is entirely the deployment's decision. The design does have a
preference, in the Assumptions table and §6: a per-user location rather than
`/usr/local/bin` or `Program Files`, because a process can only replace its own
executable if it owns the directory, and a self-update that dies with "permission
denied" on a locked-down corporate machine is a real and common failure. A
system-wide install needs elevation handling that is out of scope. What the package
does do is classify that failure honestly — `swapError` gives permission denial its
own `ClassPermissionDenied` rather than folding it into `swap_failed`. See
[apply and swap](apply-swap.md).

`Poller.target` resolving symlinks matters: an install where `/usr/local/bin/app`
symlinks into a versioned directory would otherwise have its *symlink* replaced by
a real file, quietly breaking whatever manages that layout. If `EvalSymlinks`
fails, the absolute path is used as-is — a target that does not exist yet is a first
install, not an error.

### Files alongside the target

| Path | Written by | Removed by | Lifetime |
|---|---|---|---|
| `<target>` | `Apply` (rename) | — | the live executable |
| `<target>.download` | `Downloader.Fetch` | `Poller.apply`'s defer, and by `Fetch` itself on hash/size mismatch | one cycle; may survive *within* a cycle as a resumable partial |
| `<target>.new` | `DecompressFile` | `Poller.apply`'s defer; `DecompressFile` on its own failure | one cycle, consumed by `Apply`'s rename |
| `<target>.old` | `Apply` → `retainPrevious` (hard link, or a copy fallback) | `RemoveOld`, called from `Poller.MarkHealthy`; consumed by `RestoreOld` | one generation (§3) |
| `<target>.broken` | Windows `restoreOld` only | best effort, on a later pass | see [Windows platform notes](../platforms/windows.md) |

Both staging suffixes are derived from the target, so they land in the target's own
directory — [invariant 4](invariants.md).

## What you should expect to find

**Steady state, healthy.** `install-id`, `update.lock` (empty), no marker. No
`.download`, no `.new`. `.old` may or may not exist: it is present between a swap
and the next successful `MarkHealthy`, and absent after.

**Mid-cycle** (another process is updating right now): `.download` growing, `.new`
appearing briefly, and the lock file held — invisible in a directory listing, since
`Release` never unlinks it and the lock is advisory state on the open handle.

**After an applied update, before the new binary reports healthy.**
`update-pending.json` with `attempts` at 0 (written by the old process) or 1 (the
new process has started and taken its chance), plus `<target>.old`. This is the
state a rollback consumes. See [rollback](rollback.md).

**After a rollback.** The marker is gone — `revert` clears it unconditionally, even
if the restore failed, because a surviving marker would make every subsequent start
attempt another revert. `<target>.old` is also gone: `RestoreOld` renamed it back
onto `<target>`. So a machine that has rolled back looks, on disk, exactly like a
healthy steady state; the evidence is in the local log and the
`OutcomeRolledBack` telemetry event, not the filesystem.

**A leftover `.download` with no lock activity.** A cycle failed and its cleanup
did not run — the process was killed between the write and the defer.
`resumeOffset` will either resume it or discard it on the next cycle depending on
its size, so it is self-healing; see
[download and resume](download-and-resume.md).

**A leftover `.new`.** Same cause. Nothing resumes it, and the next cycle's
`os.Remove(staged)` clears it. It is never trusted: `DecompressFile` truncates via
`os.Create`, and nothing reads `.new` except the `Apply` immediately following the
decompression that produced it.

## Debugging tips grounded in the code

- `attempts` in the marker is the count of *starts observed*, incremented and
  persisted by `CheckStartup` before any startup work. `attempts: 2` with
  `MaxAttempts` at the default 1 should not persist — that combination means a
  revert was due and either fired (marker deleted) or the marker write raced.
- `applied_at` is diagnostics only and never part of the revert decision. Do not
  read a stale timestamp as "the revert window expired"; there is no window. See
  [rollback](rollback.md).
- A failed relaunch leaves marker present, `.old` present, and the process still
  running the *old* version: `MarkPending` is the last thing `Poller.apply` does, and
  `Poller.UpdateOnce` logs a relaunch failure and stays on the old image rather than
  exiting. The new binary's one attempt is then consumed by whenever it actually
  starts.
- `cmd/app`'s `state_dir` config field points the whole thing at a scratch directory,
  which is how the demo app is driven against a local release host.

## Related

- [locking](locking.md)
- [rollback](rollback.md)
- [lifecycle ordering](lifecycle-ordering.md)
- [unix platform notes](../platforms/unix.md) and [Windows platform notes](../platforms/windows.md)
