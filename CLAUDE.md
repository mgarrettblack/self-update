# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The **client** half of a self-updating Go application: it fetches a signed release
manifest, verifies it against a compile-time trust set, replaces its own binary
atomically, and rolls back if the new build never reports healthy.

The **release** side (key generation, building, compressing, signing, hosting) is a
separate service this repo does not own. `self-update-design.md` is the spec;
`README.md` documents the HTTP contract the client expects. The design's `cmd/releaser`
and `cmd/devserver` (phase 7) are **not implemented**, so producing a signed release
means hand-rolling one with tools like `openssl` and `zstd` rather than using a tool in
this repo.

Module path is `self-update` (no domain prefix). Go 1.26.

**There is no test suite.** It was deleted wholesale as the first step of a
restructuring and its replacement has not landed. Treat `go vet` passing as "it
compiles", not as "it works", and be correspondingly careful with the invariants below.
There is currently no documented way to exercise the update cycle by hand; that gap is
open until the suite is back.

## The `docs/` tree

Everything below this section is the summary. `docs/` holds 33 per-topic files with the
detail — deliberately fragmented so a task pulls in the two or three that bear on it
rather than the whole tree. Each file opens with a `**When:**` line; the triggers below
are the compressed form of those lines.

Three are worth reading *before* you touch the thing they describe, not after:
[`update-cycle/invariants.md`](docs/update-cycle/invariants.md),
[`security/verification-order.md`](docs/security/verification-order.md), and
[`platforms/known-gaps.md`](docs/platforms/known-gaps.md).

**`docs/architecture/`** — what lives where

| Doc | When |
| --- | --- |
| [overview.md](docs/architecture/overview.md) | Starting any task and you don't yet know which file owns the behaviour |
| [package-selfupdate.md](docs/architecture/package-selfupdate.md) | Which of the nine files owns something; what a zero-valued struct field means |
| [release-go.md](docs/architecture/release-go.md) | Changing what a release *is*; navigating `release.go`'s five sections |
| [versioning.md](docs/architecture/versioning.md) | Choosing a `Version` ldflag; debugging a manifest wrongly accepted or refused |
| [dependency-rules.md](docs/architecture/dependency-rules.md) | Adding an import; deciding which file a new helper belongs in |
| [cmd-app.md](docs/architecture/cmd-app.md) | Integrating the library; driving the demo against a local host |

**`docs/update-cycle/`** — one file per stage

| Doc | When |
| --- | --- |
| [invariants.md](docs/update-cycle/invariants.md) | **Before** reordering, inlining or adding any step in `Update` / `apply` |
| [lifecycle-ordering.md](docs/update-cycle/lifecycle-ordering.md) | Wiring into an app's `main`; moving any `Startup` / `MarkHealthy` / `Run` call |
| [check.md](docs/update-cycle/check.md) | Editing `Checker`; debugging a spurious "no update" |
| [rollout-cohorts.md](docs/update-cycle/rollout-cohorts.md) | Editing `InRolloutCohort`; "why did this client not get the release" |
| [download-and-resume.md](docs/update-cycle/download-and-resume.md) | Editing `net.go`; a resumed, truncated or endlessly-retrying download |
| [decompress.md](docs/update-cycle/decompress.md) | Editing `DecompressFile`; a `decompression` failure or a wrong-size `.new` |
| [apply-swap.md](docs/update-cycle/apply-swap.md) | Editing `Apply`; a `swap_failed` failure or a stray `.old` |
| [relaunch.md](docs/update-cycle/relaunch.md) | Editing `Relaunch`; handling `ErrRestartRequired`; code that runs after a relaunch |
| [rollback.md](docs/update-cycle/rollback.md) | Editing `Guard`; a revert that didn't fire, fired twice, or shouldn't have |
| [locking.md](docs/update-cycle/locking.md) | Editing `AcquireLock`; a `locked` outcome or a stale `update.lock` |
| [disk-space.md](docs/update-cycle/disk-space.md) | Editing `ensureFreeSpace`; a `disk_full` on a disk that looked fine |
| [state-and-markers.md](docs/update-cycle/state-and-markers.md) | Inspecting an installation on disk; "what should be there right now" |

**`docs/security/`** — the controls and what each one prevents

| Doc | When |
| --- | --- |
| [verification-order.md](docs/security/verification-order.md) | **Before** touching anything between "fetch the manifest" and "swap the binary" |
| [trust-model.md](docs/security/trust-model.md) | Editing the trust set or verifier; adding any new way to learn a public key |
| [key-management-and-rotation.md](docs/security/key-management-and-rotation.md) | Rotating a signing key; responding to a suspected key compromise |
| [fail-closed-points.md](docs/security/fail-closed-points.md) | Before adding a fallback, or making an existing failure non-fatal |
| [threat-model.md](docs/security/threat-model.md) | Judging whether a change weakens a guarantee; triaging a tamper report |

**`docs/release-contract/`** — the external service this client consumes

| Doc | When |
| --- | --- |
| [http-endpoints.md](docs/release-contract/http-endpoints.md) | Standing up or debugging a release host; changing URLs, headers, status handling or resume |
| [manifest-schema.md](docs/release-contract/manifest-schema.md) | Adding a manifest field; a manifest rejected as `manifest_invalid` |
| [signature-format.md](docs/release-contract/signature-format.md) | Debugging `signature_invalid`; writing a service that signs manifests |
| [artifact-format.md](docs/release-contract/artifact-format.md) | Changing compression, the decompressed-size bound, or the space preflight |

**`docs/observability/`** and **`docs/platforms/`**

| Doc | When |
| --- | --- |
| [error-classes.md](docs/observability/error-classes.md) | Adding an error path; decoding an unfamiliar `ErrorClass` from the field |
| [telemetry-events.md](docs/observability/telemetry-events.md) | Changing `Event`; wiring ingestion; what does and doesn't leave the machine |
| [unix.md](docs/platforms/unix.md) | Editing `fs_unix.go` |
| [windows.md](docs/platforms/windows.md) | Editing `fs_windows.go`; why the swap isn't a single rename |
| [build-tags.md](docs/platforms/build-tags.md) | Adding platform-specific behaviour; reaching platform code from the wrong host |
| [known-gaps.md](docs/platforms/known-gaps.md) | **Before** relying on any claim that a part of this code has been verified |

When you change behaviour, update the doc that owns it — its `**Source of truth:**` line
names the code it tracks, and the code wins whenever they disagree.


`$PUBKEY` is a comma-separated list of standard-base64 Ed25519 public keys. The demo
app takes a single `-env <path>` flag (default `.env.local`) naming a dotenv file with
`manifest_url`, `target`, `state_dir` and `interval` fields for driving it against a
release host. See `.env.local` at the repo root for a commented example, and
[docs/architecture/cmd-app.md](docs/architecture/cmd-app.md) for the full field table.

## Architecture

One library package, `internal/selfupdate`, plus `cmd/app` — a demo integration that
exists to document the required call ordering. The library depends only on `zstd` and
`x/sys`.

Its files are grouped by **distance from the OS**, not by topic, so that tracing one
update cycle means moving down the stack rather than hopping between eight files:

| File | Layer |
| --- | --- |
| `doc.go` | the layer map and the ordering invariants |
| `constants.go` | every tunable, in sections mirroring the layers |
| `errors.go` | `ErrorClass`, `Error`, `classify`/`classifyf`, `ClassOf` |
| `fs.go` | the OS: lock, free space, binary swap, relaunch, decompress, state dir |
| `fs_unix.go`, `fs_windows.go` | the OS, per-platform syscalls |
| `net.go` | HTTP: `fetchBytes`, `requireHTTPS`, the `Downloader` |
| `release.go` | release data, pure: semver, manifest, signatures, trust set, rollout |
| `update.go` | orchestration: `Checker`, `Guard`, `Poller` |
| `telemetry.go` | cross-cutting |

Dependencies run strictly downward: `update.go` → `net.go`/`release.go`/`fs.go` → the
platform files. `errors.go` and `constants.go` are depended on by everything and depend
on nothing. A new call that runs *up* this list is the signal that something is in the
wrong file.

`release.go` is the top of the OS-distance ordering despite being a leaf in the
dependency graph: nothing in it touches a syscall, the network or the clock. Keep it
that way — it is the only part of the package that can be reasoned about in isolation.

### The one ordering that matters

`Poller.Startup()` → app's real startup → `Poller.MarkHealthy()` → `go Poller.Run(ctx)`.

`MarkHealthy` is what deletes the crash-loop marker and the retained `.old` binary.
Calling it before the startup work that can fail defeats rollback entirely. Everything
in `update.go`'s attempt accounting assumes this placement.

### The update cycle (`update.go`)

`Update` → `Checker.Check` → optional `RequireConfirmation` → `Poller.apply`, which
under a `flock` held for the whole cycle does: space preflight → download to
`<target>.download` → decompress to `<target>.new` → `Apply` (swap) → `MarkPending` →
relaunch. Staging files are removed on every exit path.

Invariants that constrain any change to this sequence:

1. **Verify before parse.** The signature is checked over the raw manifest bytes as
   received, before JSON decoding. Nothing untrusted reaches a parser.
2. **Verify before decompress.** SHA-256 covers the *compressed* bytes — the ones the
   signed manifest describes. `DecompressFile` only ever runs on a file `Fetch` already
   verified.
3. **Never execute unverified bytes.** `Apply` documents this as a precondition it does
   not itself check.
4. **Staging files live in the target's directory.** `Apply` rejects a cross-directory
   source rather than falling back to a copy: a cross-volume rename is not atomic.
5. **Marker after the swap, before the relaunch.** Earlier fires a revert for an update
   that never happened; later never runs at all, because unix `Relaunch` does not return.
6. **Telemetry is drained before the relaunch**, for the same reason.
