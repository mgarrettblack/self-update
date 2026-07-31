# cmd/app — the demo integration

**When:** Integrating this library into an application and needing the reference call ordering, editing `cmd/app/main.go`, or driving the client by hand against a local release host.

**Source of truth:** `cmd/app/main.go` — the only file in the command. There is no `main_test.go`. The code wins over this doc.

---

## What it is, and is not

`cmd/app` is a demonstration application that keeps itself up to date. It exists to
document the **shape of the integration**, which is almost entirely about ordering. It is
not a product, not a supported CLI, and its flags exist to make the client drivable against
a local release host during development.

`appName` is the constant `"demoapp"`, which is what `DefaultStateDir` namespaces on and
what the `User-Agent` (`demoapp/<version>`) is built from.

## The ordering, which is the whole point

`main` is four lines: install a signal-cancelled context and call `run`. `run` calls
`setup` — which parses `-demo`/`-version`, loads the config file and constructs the
poller — and returns immediately with `setup`'s exit code if there's nothing left to
do (help, `-version`, or a setup failure). Everything after that reads top to bottom
in one screen, in order:

1. **`poller.Startup()`**, first, before any work that could plausibly crash — the point is
   to run in the part of the program too simple to *be* the thing that crashes. If the
   previous post-update start never reported healthy, this restores the retained binary and
   relaunches into it; on unix it does not return. A failure here is logged at error level and
   **never fatal**: a broken marker or a failed revert must not stop the application from
   starting.
2. **The application's own startup**, marked in the source with
   `// === put your application's own startup here ===` … `// === end application startup ===`.
   In the demo this is one log line.
3. **`poller.MarkHealthy()`**, and not before. Reporting healthy is what discards the
   crash-loop marker and the retained `.old` binary, so it has to come after the parts of
   startup that can fail — otherwise crash-loop protection is defeated by construction.
   Also logged at error level on failure, not fatal.
4. **`poller.Run(ctx)`** for the life of the process.

`setup` exists only to keep this ordering readable — everything it does (flag parsing,
config loading, poller construction) happens *before* step 1, and none of it is part of
the ordering invariant itself. See [package-selfupdate.md](package-selfupdate.md) for
why the invariant lives in the numbered steps above, not in `setup`.

Note that the methods called here are on **`Poller`**, not `Guard`: `Poller.Startup` and
`Poller.MarkHealthy` resolve the target binary and then delegate to `Guard.CheckStartup`
and `Guard.MarkHealthy`. (`doc.go` currently names these `Guard.Startup` /
`Guard.MarkHealthy`; that is a doc-comment lag — see
[overview.md](overview.md#migration-status--the-target-is-not-fully-reached).)

`Run` returning `selfupdate.ErrRestartRequired` is a **normal** outcome, not an error: an
update was applied and a successor process exists, so this one logs
"shutting down so the updated binary can take over" and exits 0. That is the Windows path;
on unix `Relaunch` replaces the process image and never returns. See
[../update-cycle/relaunch.md](../update-cycle/relaunch.md) and
[../update-cycle/rollback.md](../update-cycle/rollback.md).

`main` installs a `signal.NotifyContext` for `os.Interrupt`/`SIGTERM` rather than a handler
that exits, because the poller may be mid-download and needs the chance to remove its
staging files.

## Construction order in `newPoller`

`selfupdate.TrustedVerifier()` is called **first**, because everything else is pointless
without it — and unlike a failed check, a trust-set problem is **fatal** (exit 1): this
build can never update itself, and that should be loud. Then the state dir (`state_dir`
or `DefaultStateDir(appName)`), then `InstallID(stateDir)`, then the `Poller` literal. The
`Reporter` is attached only if `telemetry_url` is set, and `RequireConfirmation` only under
`confirm`. See [../security/fail-closed-points.md](../security/fail-closed-points.md).

The demo leaves `MaxStartAttempts`, `TargetPath` (unless `target`), `LockPath`, `Argv` and
`Relaunch` at their zero values, which is the intended way to configure this library — see
[package-selfupdate.md](package-selfupdate.md#zero-values-are-the-configuration).

## Flags and config

The demo takes two CLI flags and reads everything else from a YAML file:

| Flag | Default | What it is for |
| --- | --- | --- |
| `-demo <path>` | `demo_config.yml` | Path to the YAML config file described below. |
| `-version` | false | Print `demoapp <version> (<os-arch>)` and exit 0. The quickest way to confirm the version ldflag landed. |

The config file's fields (see `demo_config.yml` at the repo root for a fully-commented
example):

| Field | Default | What it is for |
| --- | --- | --- |
| `manifest_url` | — | **Required.** Full URL of the signed manifest. The `.sig` URL is derived by appending `.sig`, never configured — that removes a way to misconfigure a client into verifying release A's manifest against release B's signature. |
| `insecure` | false | Sets **both** `AllowInsecureManifestURL` and `AllowInsecureArtifactURL`, and is the only way to point the client at plain HTTP. For a local release host only: over plaintext a network attacker can suppress an update indefinitely by serving a stale manifest, which signatures do not prevent. The library keeps the two switches independent so a dev setup can serve the manifest locally while still enforcing real policy on what gets executed; the demo collapses them into one field for convenience. |
| `confirm` | false | Installs a `RequireConfirmation` func that prompts on stdin (`Update available: X -> Y. Install now? [y/N] `). Anything other than an explicit `y`/`yes` declines, **EOF included** — an unattended process whose stdin is closed has not consented to anything. The prompt fires after the release is verified as applicable and *before* anything is downloaded: consent that arrives once the bytes are on disk is not much of a choice. |
| `state_dir` | `DefaultStateDir("demoapp")` | Where the lock, install id and rollback marker live. Overridable so a second local instance gets its own state — two copies sharing a state dir share a lock file *and* an install id, which changes both single-instance behaviour and rollout cohorting. See [../update-cycle/state-and-markers.md](../update-cycle/state-and-markers.md). |
| `target` | the running executable | Executable to replace, for driving a swap against a throwaway binary instead of the live process. |
| `telemetry_url` | "" (disabled) | Endpoint for update telemetry; empty leaves `Poller.Reporter` nil, which disables reporting entirely. See [../observability/telemetry-events.md](../observability/telemetry-events.md). |
| `interval` | 1h | Base poll period, before jitter. Written as a plain duration string (`"1h"`). |

## Exit codes

| Code | Constant | Meaning |
| --- | --- | --- |
| 0 | `exitOK` | Normal exit, `-version`, `-h`, or `ErrRestartRequired` |
| 1 | `exitRuntimeError` | Fatal configuration/trust-set problem from `newPoller`, or `Run` returned a real error |
| 2 | `exitUsageError` | Flag parsing failed, the config file couldn't be read/parsed, or `manifest_url` was missing |

## Building it

Both ldflags are required, and both now target the `selfupdate` package — the doc comment
at the top of `main.go` carries the same line. See
[dependency-rules.md](dependency-rules.md#the-two-required-build-ldflags) for what breaks
without each.

```sh
go build -ldflags "\
  -X self-update/internal/selfupdate.Version=1.4.2 \
  -X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
```

There is no in-repo way to produce a real signed release — the design's `cmd/releaser` and
`cmd/devserver` (phase 7) are not implemented, and the test suite that used to fabricate
signed releases has been deleted. Driving this end to end therefore needs an external
release host serving the contract in
[../release-contract/http-endpoints.md](../release-contract/http-endpoints.md), with
`insecure: true` in the config if it is plain HTTP.
