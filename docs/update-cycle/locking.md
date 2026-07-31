# The single-instance update lock

**When:** Editing `Lock` / `AcquireLock` in `internal/selfupdate/fs.go` or the `lockFile` / `unlockFile` pair in `fs_unix.go` / `fs_windows.go`, changing where the lock is acquired or released in the cycle, or debugging a `locked`-class outcome or a stale `update.lock` file.

**Source of truth:** `internal/selfupdate/fs.go` (`ErrLocked`, `Lock`, `AcquireLock`, `Release`), `internal/selfupdate/fs_unix.go` and `fs_windows.go` (`lockFile`, `unlockFile`), `internal/selfupdate/update.go` (`Poller.apply`, `Poller.LockPath`), `internal/selfupdate/constants.go` (`lockFilename`, `lockFileMode`, `lockDirMode`), `self-update-design.md` §2. The code wins if this document disagrees with it.

---

```go
AcquireLock(path string) (*Lock, error)   // non-blocking
(*Lock).Release() error                   // idempotent
var ErrLocked = errors.New("another instance holds the update lock")
```

Default path is `<state-dir>/update.lock` (`lockFilename`), overridable via
`Poller.LockPath`. The state directory is namespaced by app name
(`DefaultStateDir`), so two applications using this package never share a lock
file.

## Held for the whole cycle

`Poller.apply` resolves the path, takes the lock as its first real action, and
releases it in a `defer`:

```go
lockPath := p.LockPath
if lockPath == "" {
    lockPath = filepath.Join(p.StateDir, lockFilename)
}
lock, err := AcquireLock(lockPath)
...
defer func() {
    os.Remove(compressed)
    os.Remove(staged)
    lock.Release()
}()
```

So the lock spans: space preflight → download → decompress → swap →
`MarkPending`. Note the cleanup order inside the defer — **both staging files are
removed before the lock is released**, so no other instance can observe the
half-cleaned state in between.

The lock is *not* held across `Checker.Check` (read-only, harmless to run
concurrently) and *not* held across the relaunch, which happens back up in
`Poller.Update` after `apply` has returned and the defer has run. It is
therefore released between cycles rather than held for the process lifetime, so a
long-lived instance does not starve every other copy on the machine forever.

`Poller.apply` creates `p.StateDir` at `0o700` (`stateDirMode`) before acquiring,
because the state directory may not exist on a first run. `AcquireLock` also
creates the lock file's parent itself, at `0o755` (`lockDirMode`) — a wider mode,
because that path only matters when a caller has pointed `LockPath` somewhere
outside the per-user state directory, and the lock file carries nothing sensitive.

## What it prevents

Multiple copies of the application can be running at once — that is an explicit
assumption in the design. Only one of them may perform the download-and-swap:

- Both instances stage to **the same paths**, `<target>.download` and
  `<target>.new`, because those paths are derived from the target and must live in
  the target's directory ([invariant 4](invariants.md)). Two concurrent downloads
  would interleave writes into one file and produce a binary that matches no digest
  at all.
- Both would race on the same directory entry during the swap, and the loser could
  rename its own half-verified file over a binary the winner just replaced.

Instances that fail to take the lock simply do nothing. They notice the version
bump and pick up the new binary on their own next restart, so contention costs
nothing but a deferred update.

## Contention is not failure

```go
errors.Is(err, ErrLocked)  // "someone else is updating, this is fine, try next cycle"
```

`AcquireLock` returns an error wrapping `ErrLocked`, classified `ClassLocked`.
Callers must distinguish it from a real problem (bad state directory, no
permission), which is why it is a sentinel and not just a class. `ClassLocked` is
not a tamper signal and reports at `SeverityWarn`; a fleet showing a steady trickle
of `locked` is normal for an app users run several copies of.

Acquisition never blocks. Both platform implementations request the lock with a
fail-immediately flag, so a contended update is skipped rather than stalling the
process.

## What it is not

- **Not a defence against a hostile local process.** The lock is *advisory*; it
  coordinates cooperating copies of this program.
- **Not a goroutine mutex.** Use it once, at the top of the update cycle. On unix
  the flock is attached to the open file description, so a second `open` of the
  same path *from this same process* genuinely conflicts — which is what makes it
  usable as a single-instance guard with no process-level bookkeeping, and also
  means a second `AcquireLock` in one process fails rather than nesting.

`Release` itself is safe for concurrent calls (a mutex around the `*os.File`, which
is nil'd on the way out), safe on a nil receiver, and safe to call twice. That is
about surviving a messy shutdown path, not about lending the lock any mutex
semantics.

## Crash safety

Crashing without calling `Release` is safe: the OS drops flock / `LockFileEx` state
when the file handle is closed by process teardown, so a killed updater does not
leave a lock that must be manually cleared. That is the reason for using a real file
lock rather than a "does a pidfile exist" check — a pidfile survives a `SIGKILL`
and wedges every future update until someone deletes it by hand.

## The lock file is never deleted

`Release` unlocks and closes, but deliberately does **not** unlink:

> Unlinking it would remove the directory entry while another instance may already
> be blocked on — or about to open — that path: the next acquirer would create a
> fresh inode and lock that instead, so two instances could each hold "the" lock on
> two different files with the same name.

A single empty file left behind in the state directory is a much cheaper outcome
than a swap race. So an `update.lock` present on disk means nothing about whether an
update is in progress — do not read it as a stale-lock symptom.

`AcquireLock` opens with `O_CREATE|O_RDWR` and *not* `O_TRUNC`: truncating would
mutate a file another instance currently holds locked, and there is nothing in it to
clear. The file's contents are never read or written — only its existence and its
advisory lock state matter. Mode is `0o644` (`lockFileMode`).

`Release` unlocks explicitly before closing. `Close` alone would release the lock on
every supported platform, but being explicit keeps the error visible if the unlock
itself fails; the two errors are joined rather than one shadowing the other.

## The platform split

| | unix (`fs_unix.go`) | Windows (`fs_windows.go`) |
|---|---|---|
| primitive | `unix.Flock(fd, LOCK_EX\|LOCK_NB)` | `windows.LockFileEx(..., LOCKFILE_EXCLUSIVE_LOCK\|LOCKFILE_FAIL_IMMEDIATELY, ...)` |
| scope | whole file, via the open file description | one byte at offset 0 (`lockRegionLength = 1`) |
| contention appears as | `EWOULDBLOCK` **or** `EAGAIN` | `ERROR_LOCK_VIOLATION`, or `ERROR_IO_PENDING` |
| unlock | `LOCK_UN` | `UnlockFileEx` with the identical region |

Both map contention onto `ErrLocked` and pass everything else through for
`ClassOf` to classify.

Both error checks are wider than they look necessary, on purpose. On Linux `EAGAIN`
and `EWOULDBLOCK` are the same value; on some BSDs they are not, so both are checked
rather than switched on. On Windows, `ERROR_IO_PENDING` should not occur for a
non-overlapped handle, but it is treated as contention too: if the call did somehow
queue, this instance does not hold the lock and must not act as though it does.

Windows has no whole-file lock primitive, hence the fixed one-byte region — the
convention works because every instance agrees to lock exactly that region. The
region need not exist in the file (locking past end-of-file is legal), which is why
the lock file can stay empty. `lockRegionLength` stays in `fs_windows.go` rather
than moving to `constants.go`, along with the other constants that are not shared
across platforms.

## Related

- [invariants](invariants.md)
- [state and markers](state-and-markers.md)
- [platform splits](../platforms/build-tags.md)
