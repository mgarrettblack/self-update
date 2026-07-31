# Relaunching into the new binary

**When:** Editing `Relaunch` in `internal/selfupdate/fs.go` or the `execProcess` implementations in `fs_unix.go` / `fs_windows.go`, handling `ErrRestartRequired` in an application, or writing code that runs after a `Relaunch` call.

**Source of truth:** `internal/selfupdate/fs.go` (`Relaunch`, `execProcess`), `internal/selfupdate/fs_unix.go` (`execReplace`, `RelaunchReplacesProcess`), `internal/selfupdate/fs_windows.go` (`execSpawn`, `RelaunchReplacesProcess`), `internal/selfupdate/update.go` (`Poller.Update`, `Poller.relaunch`, `ErrRestartRequired`), `self-update-design.md` §2. The code wins if this document disagrees with it.

---

```go
Relaunch(path string, argv []string) error
const RelaunchReplacesProcess bool   // true on unix, false on Windows
```

`Relaunch` restarts the updated binary at `path` with `argv` (`argv[0]`
included). **Whether it returns on success is platform dependent**, which is what
`RelaunchReplacesProcess` exists to tell callers.

## Unix: `exec`, and it does not return

`execReplace` is `syscall.Exec(path, argv, env)`. `RelaunchReplacesProcess` is
`true`, and the constant's comment states the consequence plainly: a nil return is
impossible and **any code after a successful `Relaunch` is dead**.

`execve(2)` is preferred over fork+exec because it preserves the PID, the
controlling terminal, open standard streams, and the parent's place in any process
group or supervisor's bookkeeping. Spawning a child and exiting the parent instead
would make an init system (systemd, launchd) see the service as having exited, and
would briefly leave two processes alive.

This is why [invariants](invariants.md) 5 and 6 exist: everything that must happen
— the crash-loop marker, the telemetry drain — has to happen *before* this call,
because there is no "after".

## Windows: spawn and exit

`execSpawn` builds an `exec.Cmd` and calls `Start()`, then
`cmd.Process.Release()`. `RelaunchReplacesProcess` is `false`: there is no
`execve` equivalent, so `Relaunch` starts a child and returns, and **the caller
MUST then shut down and exit** — the old process keeps `app.exe.old` open, and the
new one only gets a clean directory once the old one is gone.

Two deliberate details:

- `CREATE_NEW_PROCESS_GROUP` (`createNewProcessGroup = 0x00000200`) so a console
  Ctrl+C or Ctrl+Break aimed at the outgoing process, or the shell that launched
  it, does not also kill the process just started.
- `DETACHED_PROCESS` is **not** used: it would strip the successor of the console
  it inherited, which for a CLI means the user stops seeing output across an
  update. A new process group is enough to decouple lifetimes.

Standard streams are inherited so the successor keeps whatever console or
redirection the current process had. The child handle is released rather than
waited on: the caller is about to exit, and the successor must not become a zombie
of, or be tied to, a parent that is going away.

`exec.Cmd` is built literally rather than via `exec.Command`, because `path` is
already absolute and `argv` already carries the `argv[0]` the caller wants — there
is no PATH lookup to do and no `argv[0]` for `exec.Command` to pick on our behalf.

## `ErrRestartRequired`

```go
var ErrRestartRequired = errors.New(
    "update applied: this process must exit so its successor can take over")
```

Returned by `Poller.Run` when the caller must shut down. `UpdateResult.RestartPending`
carries the same signal out of `Update`.

On unix this is **never** returned from a successful update, because `Relaunch`
does not come back. It is the Windows path: the successor is a new process and the
outgoing one has to exit before its executable can be cleaned up.

`cmd/app` handles it by logging and returning 0 — a clean exit, not an error:

```go
case errors.Is(err, selfupdate.ErrRestartRequired):
    logf("shutting down so the updated binary can take over")
    return 0
```

## Why telemetry must be flushed first

`Poller.Update`, once `apply` has succeeded:

```go
p.Reporter.ReportSuccess(d.CurrentVersion, d.Manifest.Version)
// Drained before the relaunch, not after: on unix the relaunch replaces this
// process image, and an in-flight goroutine would be destroyed mid-request.
p.Reporter.Wait()
```

`Reporter.Report` is fire-and-forget — it spawns a goroutine and returns
immediately. `exec` replaces the entire process image, goroutines included, so a
report still in flight is simply gone: no request completes, nothing is retried
(telemetry never retries by design), and no error is raised anywhere. The success
event would be lost exactly on the machines where the update *worked*.

`Poller.Startup` does the same thing around `ReportRollback` before relaunching the
restored binary, and there the stakes are higher: the rollback event is the fleet's
only signal that a release is crashing.

`Wait` is nil-safe, so this holds whether or not a `Reporter` is configured. See
[telemetry events](../observability/telemetry-events.md). This is
[invariant 6](invariants.md).

## Preflight and argv handling

`Relaunch` does three things before handing off to `execProcess`:

1. Resolves `path` to an absolute path.
2. `os.Stat`s it, and rejects a directory. `syscall.Exec` would report `ENOENT`
   anyway, but on the unix path a failure *after* the point of no return is
   unrecoverable, so it is worth spending a stat to fail with something legible
   while that is still possible. A permission error here is
   `ClassPermissionDenied`; everything else is `ClassInternal`.
3. Copies `argv` before normalising it — the caller usually passes `os.Args`, and
   mutating that would be a surprising side effect. An empty `argv` becomes
   `[abs]`; an empty `argv[0]` is replaced with `abs`.

`execProcess` is a package var set by each platform's `init()`, which is what
keeps that argument handling platform-agnostic while the exec mechanics
(replace-in-place on unix, spawn-and-release on Windows) differ per OS. It is also
the seam that lets the preflight run with a fake exec on any host, without
actually replacing the running process. `Poller.Relaunch` is a struct field
serving the same purpose one layer up. A nil `execProcess` is a classified
`ClassInternal` error rather than a panic.

## A failed relaunch is not fatal

`Relaunch`'s doc comment: an error is always a genuine failure to start the
successor, and callers should treat it as "keep running the current process" rather
than exiting — exiting after a failed relaunch is how a self-updater turns a
cosmetic problem into an outage.

`Poller.Update` implements exactly that:

> The swap succeeded, so the update is real and the marker is in place; we simply
> could not hand over. Staying alive on the old image is strictly better than
> exiting, and the next start picks up the new one.

It logs, leaves `RestartPending` false, and the process continues on the old
image. The marker is already written, so the crash-loop accounting still applies to
whenever the new binary does eventually start.

## Related

- [invariants](invariants.md)
- [apply and swap](apply-swap.md)
- [lifecycle ordering](lifecycle-ordering.md)
- [Windows platform notes](../platforms/windows.md)
