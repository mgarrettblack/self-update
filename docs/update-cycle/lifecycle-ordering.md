# Lifecycle ordering

**When:** Wiring the updater into an application's `main`, reviewing or moving any call to `Poller.Startup`, `Poller.MarkHealthy` or `Poller.Run`, or explaining why a bad release was not rolled back.

**Source of truth:** `internal/selfupdate/update.go` (`Poller.Startup`, `Poller.MarkHealthy`, `Poller.Run`, and `Guard`), `internal/selfupdate/doc.go`, `internal/selfupdate/rollback.md`, `cmd/app/main.go`, `self-update-design.md` §3. The code wins if this document disagrees with it.

---

## The ordering

```
1. Poller.Startup()        // crash-loop check, before anything that could crash
2. ...the application's real startup...
3. Poller.MarkHealthy()    // and not before
4. go Poller.Run(ctx)      // background polling for the life of the process
```

`cmd/app/main.go` exists mainly to document this. Its `run` function is annotated
with the four steps and nothing else in that file is load bearing.

## Who owns these two calls

There are two layers, and it matters which one you are looking at.

`Guard` (in `update.go`) owns the marker file. Its methods are `MarkPending`,
`CheckStartup`, `MarkHealthy` and the internal `revert`. `Guard` knows about a
state directory and a binary path and nothing else.

`Poller` owns the integration surface. `Poller.Startup` and
`Poller.MarkHealthy` are the two calls an application makes; each resolves the
target binary, constructs a `Guard` for it via `Poller.guard`, and adds the work
that `Guard` deliberately does not do:

| Integrator calls | which calls | plus |
|---|---|---|
| `Poller.Startup()` | `Guard.CheckStartup()` | `Reporter.ReportRollback` + `Wait`, then `Relaunch` if a revert happened |
| `Poller.MarkHealthy()` | `Guard.MarkHealthy()` | `RemoveOld(target)` |

**Application code calls `Poller.Startup` and `Poller.MarkHealthy`**, which is
what the package doc comment in `doc.go` spells out as the load-bearing ordering.
There is no `Guard.Startup`: the crash-loop check on `Guard` is `CheckStartup`,
and `Guard.MarkHealthy` is the narrow marker-only version, not the one that also
drops `.old`. Reach past `Poller` to a bare `Guard` only if you are driving the
rollback machinery directly, and then you own the reporting, the relaunch and the
`.old` cleanup yourself.

## Why `Startup` goes first

`Poller.Startup` calls `Guard.CheckStartup`, which reads the crash-loop marker
from the state directory and, if the previous post-update start never reached
`MarkHealthy`, restores the retained `.old` binary. `Poller.Startup` then
relaunches into it.

It runs first because the point is to execute in the sliver of the program that
is too simple to be the thing that crashes. Any initialisation placed above it —
config parsing, a database handle, a listener bind — is code that can panic on
the new build, and a panic above `Startup` means the revert never runs. The
process then dies, restarts, dies again: a crash loop with the rollback machinery
sitting unreached three lines further down.

`Startup` is also deliberately non-fatal for the caller. `cmd/app` prints the
error and carries on. A corrupt marker or a failed revert must not stop the
application from starting — that would turn a bookkeeping problem into an
outage.

On unix, a revert does not return: `Poller.Startup` relaunches via `exec`. See
[relaunch](relaunch.md).

## Why `MarkHealthy` must come after the startup work that can fail

`Poller.MarkHealthy` does two things, in this order:

1. `Guard.MarkHealthy` — deletes the crash-loop marker.
2. `RemoveOld(target)` — deletes the retained `<target>.old` binary.

Both are irreversible. After the marker is gone, the next start looks like an
ordinary start and no revert will ever be considered. After `.old` is gone there
is nothing to revert *to*: `RestoreOld` fails with `ClassSwapFailed` and the
message "no retained generation".

So `MarkHealthy` is not a status report. It is the commit point of the update.
Calling it in the first line of `main` — or anywhere above the work that could
plausibly fail — does not weaken crash-loop protection, it removes it entirely
and by construction:

- the marker is deleted before the code that would crash has run,
- the `.old` binary is deleted at the same moment,
- the process then crashes, and the next start sees a clean state directory,
- every subsequent start crashes identically, with no marker and no retained
  generation, forever.

That is a bricked install that the updater itself can no longer repair, because
the crashing build never gets far enough to poll for the fix.

The rule from `MarkHealthy`'s own doc comment: call it once the application has
finished the startup work that could plausibly fail — after the config is loaded
and the listeners are up, not in the first line of `main`.

## The attempt accounting depends on this placement

`Guard`'s accounting is only correct because `MarkHealthy` sits after the risky
work. `MarkPending` writes `Attempts=0`; every `CheckStartup` increments and
immediately persists. With the default `MaxAttempts=1`:

- first post-update start takes `Attempts` to 1, which is not greater than the
  limit, so it proceeds — the new binary gets its one chance;
- if that start reaches `MarkHealthy`, the marker is deleted and the next start
  sees nothing;
- if it does not, the next start takes `Attempts` to 2, which exceeds the limit,
  and the revert fires.

"Reaching `MarkHealthy`" is the *only* evidence the guard has that the new build
works. Move the call earlier and every start reaches it, including the ones that
crash a millisecond later, so the marker is always cleared and the second branch
above becomes unreachable. Nothing errors; the mechanism simply never fires
again. See [rollback](rollback.md) for the full walkthrough.

`MarkHealthy` is a no-op when no marker exists, so it is safe — and intended —
to call unconditionally on every run, not just post-update ones.

## Why `Run` goes last

`Run` checks immediately rather than waiting for the first tick, so starting it
before `MarkHealthy` would let a cycle apply a *second* update while the current
one is still unproven. `MarkPending` would overwrite the outstanding marker with
a fresh `Attempts=0` for a different version pair, and the still-unproven build's
one chance would be silently reset. Applying an update on top of an unconfirmed
update also destroys the `.old` retention chain: only one generation is kept
(§3), so the binary you could roll back to is now the untrusted one.

`Run` never returns a check failure — an unreachable release host must not take
down the application the updater is supposed to be maintaining (§6). It returns
`nil` when `ctx` is done, or `ErrRestartRequired` when the caller must exit.
Callers handle that error by shutting down cleanly; see
[relaunch](relaunch.md).

## Related

- [invariants](invariants.md) — the six constraints inside one cycle
- [state and markers](state-and-markers.md) — what is on disk between these calls
- [rollback](rollback.md) — the guard's internals
