# Package internal/selfupdate

**When:** About to change updater behaviour and you need to know which of the nine files in `internal/selfupdate` owns it, or you are filling in a `Poller`/`Checker`/`Downloader` literal and need to know what a zero field means.

**Source of truth:** `internal/selfupdate/doc.go` for the intended layering, plus the files in the routing table below. `internal/selfupdate/rollback.md` is the in-package rationale for crash-loop detection. The code wins over this doc.

---

## Routing table — which file owns what

The package is one Go package in nine files, organised by distance from the OS
(see [overview.md](overview.md) for the diagram). Read down the table: the first row is
where an integrator starts, the last rows are where the syscalls live.

| File | Owns | Deeper doc |
| --- | --- | --- |
| `doc.go` | The package doc: the target layering and the three ordering invariants. No code. | — |
| `update.go` | The orchestration layer, and the only file that calls `net.go`, `release.go` and `fs.go` together. Three concerns in sequence: **`Checker`** + `Decision` + `Check` (manifest and `.sig` fetch, the newer/platform/rollout decision, size caps); **`Guard`** + `Marker` + `StartupResult` + `MarkPending`/`CheckStartup`/`MarkHealthy` (attempt accounting); **`Poller`** + `UpdateResult` + `ErrRestartRequired` + `Run`/`UpdateOnce`/`apply`/`Startup`/`MarkHealthy`/`target`. | [../update-cycle/check.md](../update-cycle/check.md), [../update-cycle/rollback.md](../update-cycle/rollback.md), [../update-cycle/lifecycle-ordering.md](../update-cycle/lifecycle-ordering.md) |
| `release.go` | The release-data layer: semver (`Semver`, `parseSemver`, `IsNewer`, `Version`), the manifest (`Manifest`, `PlatformArtifact`, `ParseManifest`, `Validate`, `RolloutPercent`, `PlatformKey`), signatures (`Verifier`, `NewVerifier`, `Verify`, `ParsePublicKey(s)`, `DecodeSignature`), the trust set (`TrustedKeysBase64`, `bakedInTrustedKeys`, `TrustedVerifier`) and `InRolloutCohort`. | [release-go.md](release-go.md), [versioning.md](versioning.md) |
| `net.go` | Everything that speaks HTTP: `fetchBytes` (manifest and `.sig`), `requireHTTPS`, `Downloader` + `Fetch` + `attempt`, resume (`resumeOffset`, `seedHashFromPrefix`, `contentRangeStart`), streaming SHA-256 (`writeArtifactBody`), `backoffDelay`, `retryableHTTPStatus`, `progressGate`. | [../update-cycle/download-and-resume.md](../update-cycle/download-and-resume.md) |
| `fs.go` | Everything that touches the OS through a portable API: `Lock`/`AcquireLock`/`Release`/`ErrLocked`; `ensureFreeSpace` and the 32 MiB `spaceSafetyMargin`; `Apply`/`OldPath`/`RemoveOld`/`RestoreOld`/`sameDirectory`/`swapError`; `Relaunch` and the `execProcess` seam; `DecompressFile` and the 1 GiB expansion cap; `InstallID` and `DefaultStateDir`. | [../update-cycle/locking.md](../update-cycle/locking.md), [../update-cycle/disk-space.md](../update-cycle/disk-space.md), [../update-cycle/apply-swap.md](../update-cycle/apply-swap.md), [../update-cycle/decompress.md](../update-cycle/decompress.md), [../update-cycle/state-and-markers.md](../update-cycle/state-and-markers.md) |
| `fs_unix.go` (`//go:build !windows`) | The unix syscalls behind `fs.go`: `lockFile`/`unlockFile` (flock), `freeSpace` (statfs), `applySwap` (rename over a running executable), `retainPrevious`/`copyFile` and the `linkFile` seam, `restoreOld`, `execReplace`, `RelaunchReplacesProcess = true`, `binaryMode` 0o755. | [../platforms/unix.md](../platforms/unix.md) |
| `fs_windows.go` (`//go:build windows`) | The Windows equivalents: `lockFile`/`unlockFile` (`LockFileEx` over a one-byte region), `freeSpace` (`GetDiskFreeSpaceEx`), `applySwap` — the rename dance with its `.broken` fallback — `restoreOld`, `execSpawn` with `createNewProcessGroup`, `RelaunchReplacesProcess = false`. | [../platforms/windows.md](../platforms/windows.md), [../platforms/known-gaps.md](../platforms/known-gaps.md) |
| `errors.go` | `ErrorClass` and its ten constants, `Error`, `classify`/`classifyf`, `ClassOf`, `IsTamperSignal`. Bottom of the layering: depends on nothing of ours. | [../observability/error-classes.md](../observability/error-classes.md) |
| `telemetry.go` | `Reporter`, `Event`, `Outcome`, `Severity`, `ReportFailure`/`ReportSuccess`/`ReportRollback`/`Report`/`Wait`. Cross-cutting and fire-and-forget. | [../observability/telemetry-events.md](../observability/telemetry-events.md) |

### Transitional notes

- **`constants.go` is in `doc.go`'s target list but does not exist.** Its would-be contents
  are still declared next to their users: `update.go` (poll interval, jitter, staging
  suffixes, manifest/signature caps, decompression ratio estimate), `net.go` (fetch
  attempts, backoff, buffer size), `fs.go` (space margin, `.old` suffix, decompression
  cap). If you add a constant, put it beside the code that reads it and give it the comment
  explaining its value — that is the convention either way.
- **`update.go` is a merge of three former files** (`check.go`, `poller.go`, `rollback.go`)
  and is 700+ lines. The three concerns appear in that order and each has a leading doc
  comment; navigate by type name rather than line number.
- **There are no `_test.go` files.** Nothing in this package is covered by a test.

## The principal types

- **`Poller`** — the only type an application needs to construct. It owns `Checker`,
  `Downloader`, `Reporter` and (internally, via `guard()`) a `Guard`, and it enforces the
  ordering that makes rollback work.
- **`Checker`** → `*Decision`. Performs no writes and downloads nothing but the manifest
  and its signature, so a failed check is always safe to retry.
- **`Downloader`** → a verified compressed artifact on disk. `Fetch` returns nil only when
  SHA-256 over the received bytes matches the signed manifest.
- **`Verifier`** — a *set* of Ed25519 public keys, never one key: a client that trusts
  exactly one key can never be migrated off it. See
  [../security/trust-model.md](../security/trust-model.md).
- **`Guard`** — the crash-loop marker state machine. `MarkPending` (old process, after the
  swap) / `CheckStartup` (new process, first thing) / `MarkHealthy` (new process, after
  real startup).
- **`Lock`** — the advisory lock held for the whole download-through-swap cycle.
- **`Reporter`** — optional telemetry sink. Nil-receiver safe, so the poller calls it
  unconditionally.

`Guard`, `Lock`, `Verifier`, `Apply`, `DecompressFile`, `Relaunch` and `ensureFreeSpace`
are all usable standalone; `Poller` is a composition of them, not a wrapper that hides
them.

## Zero values are the configuration

There are **no constructors** other than `NewVerifier` (which exists to reject an empty
trust set) and `TrustedVerifier` (which exists to reject an empty *compiled-in* trust set).
Every optional field is zero-value-meaningful, so a struct literal is the whole
configuration API and adding a field never breaks a caller:

| Field | Zero means |
| --- | --- |
| `Poller.Interval` | 1 hour (`defaultPollInterval`), plus up to 50% jitter |
| `Poller.MaxStartAttempts` | 1 — the new binary gets exactly one chance |
| `Poller.Logf` | discard; a library must not write to a program's output uninvited |
| `Poller.Downloader` | a default `&Downloader{}` |
| `Poller.Reporter` | telemetry disabled entirely (nil receiver is safe) |
| `Poller.TargetPath` | the running executable, with symlinks resolved |
| `Poller.LockPath` | `StateDir/update.lock` |
| `Poller.Argv` | `os.Args` |
| `Poller.Relaunch` | the platform `Relaunch` |
| `Poller.RequireConfirmation` | apply without asking |
| `Poller.ReportNoUpdate` | off — an hourly beacon is the operator's call, not the library's |
| `Checker.CurrentVersion` | the package-level `Version` var |
| `Checker.Platform` | `PlatformKey()` (`GOOS-GOARCH`) |
| `Checker.MaxManifestBytes` | 1 MiB |
| `Checker.Client` | a fresh client with a 30s timeout |
| `Downloader.MaxAttempts` / `BaseBackoff` | 4 attempts / 1s base, capped at 30s |
| `Downloader.Client` | a shared client with a 15 min timeout (connection reuse across polls) |
| `Downloader.Progress` | no progress callbacks |
| `Reporter.Timeout` | 5s per POST |
| `Guard.MaxAttempts` / `Now` / `Restore` | 1 / `time.Now` / `RestoreOld` |

Two fields are **required** and have no default: `Poller.Checker` and `Poller.StateDir`
(the state dir must be writable even when the install directory is not). `UpdateOnce`
returns a `ClassInternal` error if either is missing. `Checker.Verifier` is likewise
required — a nil one is a hard error before any network access. See
[../security/fail-closed-points.md](../security/fail-closed-points.md).

Follow this convention rather than adding constructors. The platform seams (`execProcess`
and `linkFile` as package vars, `Guard.Restore` and `Poller.Relaunch` as struct fields)
exist for the same reason: injectable behaviour keeps the logic exercisable on any host.

## Errors

Every failure path is tagged with an `ErrorClass` through `classify`/`classifyf`. New
error paths must be classified too — `ClassOf` defaults an untagged, unrecognised error to
`ClassInternal` specifically so a mis-inference never fakes a tamper signal. See
[../observability/error-classes.md](../observability/error-classes.md).
