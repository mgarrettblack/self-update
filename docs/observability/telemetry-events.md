# Telemetry Events

**When:** Adding or changing a field on `Event`, adding a `Report*` call site, wiring an ingestion endpoint, or answering what the client sends upstream and whether it can leak anything.

**Source of truth:** `internal/selfupdate/telemetry.go`, the `Reporter` call sites in `update.go`, and `InstallID` in `fs.go`. §5 of `self-update-design.md` is the requirement. The code wins if they disagree.

---

## `Event` — the entire wire format

```go
type Event struct {
    InstallID   string     `json:"install_id"`
    OS          string     `json:"os"`
    Arch        string     `json:"arch"`
    FromVersion string     `json:"from_version"`
    ToVersion   string     `json:"to_version,omitempty"`
    Outcome     Outcome    `json:"outcome"`
    ErrorClass  ErrorClass `json:"error_class,omitempty"`
    Severity    Severity   `json:"severity"`
}
```

Eight fields. That is the whole thing: one flat JSON object, POSTed with `Content-Type:
application/json`.

`Outcome` is an enumeration: `success`, `failure`, `no_update`, `rolled_back`.
`Severity` is `info`, `warn`, `alert`. `ErrorClass` is the ten-value enumeration in
[error-classes.md](error-classes.md). `FromVersion`/`ToVersion` are semver strings — see
[../architecture/versioning.md](../architecture/versioning.md). `OS`/`Arch` are
`runtime.GOOS`/`runtime.GOARCH`. `InstallID` is a random 16-byte hex identifier from
`InstallID` in `fs.go` — deliberately random rather than derived from hostname or
username, precisely because it is attached to every event, and persisted rather than
regenerated because rollout cohorting keys on it (see
[../update-cycle/rollout-cohorts.md](../update-cycle/rollout-cohorts.md)).

### The constraint on new fields

**Every field must stay a fixed enumeration or a version string. Nothing free-form.**
There is no field for an error message, an operation name, a path, or a URL, and adding
one is how this file's guarantee gets broken. A raw error string can contain a local path
or a username. The `*Error.Op` string from `errors.go` is not reported for the same
reason: it is built from operation names and interpolated paths, and only ever reaches the
local log.

The guarantee is structural, not asserted anywhere at runtime. `Reporter.send` marshals
`Event` and nothing else — `json.Marshal(ev)` on a struct with exactly these eight fields
is the entire body — so a field that is not on `Event` cannot reach the wire, and a field
that is added to `Event` reaches it immediately and silently. There is no test suite in
this repo to catch that (see [../platforms/known-gaps.md](../platforms/known-gaps.md)), so
the review of a new field *is* the enforcement. Read the struct, not a test name.

## `Reporter`

```go
type Reporter struct {
    Endpoint  string
    InstallID string
    Client    *http.Client  // nil means a shared default client
    Timeout   time.Duration // 0 means defaultReportTimeout, 5s
    UserAgent string

    inFlight sync.WaitGroup // unexported; joined by Wait
}
```

`enabled()` is `r != nil && r.Endpoint != ""`. Both a nil receiver and an unset
`Endpoint` mean disabled, which is why `update.go` calls `p.Reporter.ReportSuccess(...)`
unconditionally without a nil check anywhere. **Telemetry is fully optional: a nil
`Reporter` sends nothing and is not an error.** §5 notes this is the one place the
design's "no custom server" assumption gets stretched, so operating without an endpoint
has to be a first-class configuration. `cmd/app` only constructs a `Reporter` when
`telemetry_url` is given — see [../architecture/cmd-app.md](../architecture/cmd-app.md).

`Report` stamps identity itself — `InstallID`, `OS`, `Arch`, and a default `Severity` of
`info` — rather than trusting the caller, so no call site can omit them.

### The three convenience reporters

| Method | Outcome | Severity |
|---|---|---|
| `ReportSuccess(from, to)` | `success` | `info` |
| `ReportFailure(from, to, err)` | `failure` | `alert` if `ClassOf(err).IsTamperSignal()`, else `warn` |
| `ReportRollback(from, to)` | `rolled_back` | `warn` |

The severity decision for a failure is two lines inside `ReportFailure`; there is no
`severityFor` helper, because nothing else needs to make that call. A rollback is `warn`,
not `alert`, and the reasoning is worth preserving: the binary that was installed *is* the
one that was signed, it simply did not stay up. That is a bad release, not an attack.

A fourth event exists but has no convenience method. `Poller.UpdateOnce` emits an
`Outcome: no_update` / `SeverityInfo` event directly via `Reporter.Report`, and only when
`Poller.ReportNoUpdate` is set. It is off by default because it turns the fleet's hourly
poll into an hourly beacon — a useful version census and a large volume of traffic, and
that tradeoff belongs to the operator.

## Fire-and-forget

`Report` returns immediately, never blocks, never retries, and never returns an error.
It marshals and POSTs on a goroutine tracked by the unexported `inFlight` `sync.WaitGroup`,
with a `recover()` around the send so a pathological client or transport cannot take the
hosting application down. `send` swallows every error: there is nothing useful to do
with a failed telemetry write, and retrying would amplify load against an endpoint that
is already unhappy. Single attempt, deadline-bounded by `Timeout` (default 5s) so a
black-holed endpoint cannot pin a goroutine for the process lifetime. The response body
is drained up to `maxDrain` (4 KiB) purely so the keep-alive connection can be reused.

Telemetry failing must never affect the update path — the same principle as update-check
errors being non-fatal to the running app (§5, §6). See
[../security/fail-closed-points.md](../security/fail-closed-points.md) for the contrast:
the updater fails *closed* on trust and *open* on telemetry, and the two are deliberately
opposite.

## `Wait()` and the relaunch ordering

`Reporter.Wait()` joins in-flight sends. The update path calls it in exactly two places,
both immediately before a relaunch:

- `Poller.UpdateOnce` — `ReportSuccess` then `Wait` then `p.relaunch(target)`.
- `Poller.Startup` — `ReportRollback` then `Wait` then `p.relaunch(target)`.

**Drained before the relaunch, not after, because on unix `Relaunch` replaces the
process image via `execve(2)` and does not return.** Anything after it is dead code, and
an in-flight goroutine would be destroyed mid-request — the success event for an update
that did happen would simply vanish, and the fleet's telemetry would systematically
under-report exactly the outcome it most needs to count. This is one of the numbered
invariants on the update cycle; see
[../update-cycle/invariants.md](../update-cycle/invariants.md),
[../update-cycle/lifecycle-ordering.md](../update-cycle/lifecycle-ordering.md) and
[../platforms/unix.md](../platforms/unix.md).

**The doc comment on `Wait` is stale and says the opposite.** It reads "For tests and for
a clean shutdown only — the update path never calls it," which was true before the drain
ordering was introduced and is now contradicted by the two call sites above — both of
which are on the update path, and one of which (`UpdateOnce`) is the update path's
happy case. Trust the call sites. The comment has been left alone deliberately rather
than quietly corrected here; if you are in `telemetry.go` for another reason, it is worth
fixing at the source.

Any new report emitted on a path that ends in a relaunch must be followed by `Wait()`.
Conversely, a `Wait()` on a path that does *not* end in a relaunch is just a blocking
call in a background poller, which is what fire-and-forget exists to avoid — the two
existing call sites are both justified by the `execve` that follows them, and nothing
else is.

Related: [../update-cycle/relaunch.md](../update-cycle/relaunch.md) for the handoff
itself, [../update-cycle/rollback.md](../update-cycle/rollback.md) for what produces a
`rolled_back` event, and [../update-cycle/check.md](../update-cycle/check.md) for the
`no_update` path.
