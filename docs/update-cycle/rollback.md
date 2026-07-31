# Rollback and crash-loop detection

**When:** Editing `Guard` in `internal/selfupdate/update.go`, changing `MaxStartAttempts` / `MaxAttempts` semantics, or debugging a revert that did not fire, fired twice, or fired when it should not have.

**Source of truth:** `internal/selfupdate/update.go` (`Guard`, `Marker`, `MarkPending`, `CheckStartup`, `MarkHealthy`, `revert`, and the `Poller.Startup` / `Poller.MarkHealthy` wrappers), `internal/selfupdate/rollback.md` (the implementation notes that ship next to the code), `internal/selfupdate/fs.go` (`RestoreOld`, `RemoveOld`), `internal/selfupdate/constants.go` (`markerFilename`, `privateFileMode`), `self-update-design.md` §3. The code wins if this document disagrees with it.

---

Note that `internal/selfupdate/rollback.md` still opens by locating `Guard` in
`rollback.go`; that file was merged into `update.go`. Everything else in it holds.

## Lifecycle

```
old process:  Apply -> MarkPending -> Relaunch
new process:  CheckStartup -> ...real startup... -> MarkHealthy -> RemoveOld
```

The mechanism is deliberately dumb, because it has to work when the new binary is
too broken to do anything useful: a marker file written before the relaunch, and
deleted only once the new binary gets far enough to call `MarkHealthy`. A marker
still present at the next start is proof that the previous post-update start never
reached healthy — whether it panicked, was killed by the OS, hung and got
force-quit, or exited early. No timer is needed and nothing has to be observed from
inside the crashing process.

## The marker

`<state-dir>/update-pending.json` (`markerFilename`), holding a `Marker`:

```go
type Marker struct {
    FromVersion string    `json:"from_version"`
    ToVersion   string    `json:"to_version"`
    AppliedAt   time.Time `json:"applied_at"`
    Attempts    int       `json:"attempts"`
}
```

It lives in the state directory rather than next to the binary because the
binary's directory may be read-only for the user (§6) while state is always
per-user writable.

`AppliedAt` is recorded for telemetry and diagnostics — the version pair plus the
time is what makes a field report legible — and is **deliberately not part of the
revert decision**. §3 phrases crash-loop detection as "crashes within N seconds of
relaunch", but the marker's mere survival is the stronger signal: counting starts
catches a binary that dies after two minutes, or hangs and gets force-quit,
neither of which a wall-clock window would notice. A time window also depends on
the clock, which is exactly the kind of thing that is wrong on the machines that
need rollback most.

`writeMarker` writes to `<marker>.tmp` and renames, so a crash mid-write can never
leave a partial marker at the real path.

## `MarkPending`

Called by the **old** process, after `Apply` and before `Relaunch`. Writes a fresh
marker with `Attempts=0` — the new binary has not started yet, so it has not used
an attempt.

The ordering is [invariant 5](invariants.md): earlier fires a revert for an update
that never happened; later never runs at all on unix, where `Relaunch` does not
return.

## Attempt accounting

Off-by-one here is the difference between "one crash reverts" and "a revert loop".

`MarkPending` writes `Attempts=0`. Every `CheckStartup` increments and
**immediately persists** it. With the default limit of 1:

| Start | `Attempts` after increment | vs. limit | Action |
|---|---|---|---|
| first post-update start | 1 | not greater | proceed — the new binary gets its chance |
| that start reached `MarkHealthy` | — | — | marker deleted; next start sees nothing |
| otherwise, next start | 2 | exceeds | revert |

Incrementing *before* running startup work, and persisting immediately, is what
makes this work for a process that dies without ever returning control to us. If
the increment were written after startup, a process that never finishes startup
would never record its attempt, and the count would sit at 0 forever — an infinite
crash loop with no revert.

**This accounting is only correct because `MarkHealthy` sits after the startup
work that can fail.** "Reached `MarkHealthy`" is the only evidence the guard has
that the build works. Move that call earlier and every start clears the marker,
including the ones that crash immediately, and the second row of the table becomes
the only reachable one. See [lifecycle ordering](lifecycle-ordering.md).

If `writeMarker` fails during `CheckStartup`, the guard **proceeds anyway** and
returns the error alongside a non-reverted result: an unwritable state dir must not
stop the app from starting.

## `MaxAttempts` / `MaxStartAttempts`

Two names for the same knob at two layers: `Guard.MaxAttempts`, and
`Poller.MaxStartAttempts`, which `Poller.guard` passes straight through when it
constructs the `Guard`. Both treat `0` as the default of 1 — "the new binary gets
exactly one chance". Raising it trades slower recovery for tolerance of a build
that fails to start intermittently.

`Poller.guard` sets only `StateDir`, `BinaryPath` and `MaxAttempts`, so a
poller-driven `Guard` always takes the defaults for `Now` (`time.Now`) and
`Restore` (`RestoreOld`). Those two fields exist for direct `Guard` users and for
driving the revert path without a real `.old` on disk.

## Corrupt markers revert

An unparseable marker is neither fatal nor ignored:

- **Not fatal:** refusing to start would wedge the app permanently over an
  unreadable bookkeeping file, which is worse than any update failure.
- **Not "no update pending":** the file only exists at all because an update was
  applied and never confirmed healthy, and a half-written marker is itself a
  plausible symptom of the crash being looked for.

Since the attempt count is unrecoverable, the conservative reading is "an attempt
was made and failed": revert to the known-good generation. `revert(nil)` is called,
so `StartupResult.Marker` is nil while `Reverted` is true — a caller logging the
version pair must handle that, as `Poller.Startup` does.

## `revert` and `Guard.Restore`

`revert` calls `Guard.Restore` if set, otherwise `RestoreOld`, against
`g.BinaryPath` — and then clears the marker **unconditionally**, even when the
restore itself failed.

A marker left behind would make every subsequent start attempt another revert,
which is exactly the revert loop the accounting exists to avoid. Better to run the
new binary and report the failure.

The two failures are reported distinctly: a failed restore returns
`Reverted: false` with the restore's own class, while a successful restore whose
marker clear failed returns `Reverted: true` and the clear's error — the binary on
disk did change, and the caller needs to know that even though something went
wrong.

## Restoring the retained `.old` binary

`RestoreOld` fails with `ClassSwapFailed` if `<target>.old` does not exist — there
is then nothing to roll back to. On unix it is a plain `rename` (atomic, safe over
the running executable, same reasoning as the forward swap) followed by a `chmod` to
`0o755`. On Windows the running image has to be moved aside to `.broken` first; see
[Windows platform notes](../platforms/windows.md). Only one generation is retained
(§3), so exactly one rollback is possible.

**A successful restore does not by itself revert anything the process is running.**
The process is still executing the *new* image while the *old* one now sits at
`target`. `StartupResult.Reverted == true` means only that the binary on disk is no
longer the one this process is executing. The caller must `Relaunch` — or exit for
its supervisor to restart it — or it just re-runs the code that was reverted for
being broken. `Poller.Startup` relaunches, and on unix does not return.

## `MarkHealthy` and telemetry

`Guard.MarkHealthy` deletes the marker and is a no-op when there is none, so it is
safe to call unconditionally at the end of startup on every run. `Poller.MarkHealthy`
wraps it and then calls `RemoveOld(target)` — "only once no revert is possible or
wanted". On Windows that delete is also what finally removes the outgoing `.exe` the
previous process was holding open.

`Poller.Startup` reports a rollback with `Reporter.ReportRollback(from, to)` and then
`Wait()`s before relaunching, for the reason in [invariant 6](invariants.md). A
rollback is `OutcomeRolledBack` at `SeverityWarn` — a warning, because crash-loop
detection fired, but **not** a tamper signal: the binary installed is the one that
was signed, it simply did not stay up.

## Related

- [lifecycle ordering](lifecycle-ordering.md) — the one thing that must not move
- [state and markers](state-and-markers.md) — on-disk state while debugging
- [rollout cohorts](rollout-cohorts.md) — the other half of §3
- [apply and swap](apply-swap.md)
