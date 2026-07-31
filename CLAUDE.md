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
means the `openssl`/`zstd` recipe below rather than a tool in this repo.

Module path is `self-update` (no domain prefix). Go 1.26.

**There is no test suite.** It was deleted wholesale as the first step of a
restructuring and its replacement has not landed. Treat `go vet` passing as "it
compiles", not as "it works", and be correspondingly careful with the invariants below.
Until the suite is back, changes to the update cycle should be exercised by hand with
the recipe under "Driving it end to end".

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
| [invariants.md](docs/update-cycle/invariants.md) | **Before** reordering, inlining or adding any step in `UpdateOnce` / `apply` |
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

## Commands

```sh
go build ./... && go vet ./...
GOOS=windows GOARCH=amd64 go vet ./...   # the only check the Windows path gets
gofmt -l .                               # must print nothing
```

Building requires **both** linker flags. A build without them either fails to compare
versions (`Version` defaults to `0.0.0-dev`, which is valid semver but never newer) or
refuses to start its updater at all (empty trust set):

```sh
go build -ldflags "\
  -X self-update/internal/selfupdate.Version=1.4.2 \
  -X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
```

`$PUBKEY` is a comma-separated list of standard-base64 Ed25519 public keys. The demo
app takes a single `-demo <path>` flag (default `demo_config.yml`) naming a YAML file
with `once`, `insecure`, `confirm` and `state_dir` fields for driving it against a
local release host; `insecure` is the only way to point it at plain HTTP. See
`demo_config.yml` at the repo root for a commented example, and
[docs/architecture/cmd-app.md](docs/architecture/cmd-app.md) for the full field table.

### Driving it end to end

The repo has no releaser, but `openssl` and `zstd` are enough to fabricate a release
the client genuinely accepts. This is the only way to exercise the happy path right
now, and it is worth doing for any change to the update cycle:

```sh
openssl genpkey -algorithm ed25519 -out /tmp/sk.pem
PUB=$(openssl pkey -in /tmp/sk.pem -pubout -outform DER | tail -c 32 | base64)
LD="-X self-update/internal/selfupdate.TrustedKeysBase64=$PUB"

mkdir -p /tmp/host /tmp/install
go build -o /tmp/install/demoapp -ldflags "-X self-update/internal/selfupdate.Version=1.0.0 $LD" ./cmd/app
go build -o /tmp/new             -ldflags "-X self-update/internal/selfupdate.Version=9.9.9 $LD" ./cmd/app
zstd -q -f /tmp/new -o /tmp/host/a.zst

SHA=$(shasum -a 256 /tmp/host/a.zst | cut -d' ' -f1); SZ=$(stat -f%z /tmp/host/a.zst)
printf '{"version":"9.9.9","rollout":100,"platforms":{"%s":{"url":"http://127.0.0.1:8099/a.zst","sha256":"%s","size":%s}}}' \
  "$(go env GOOS)-$(go env GOARCH)" "$SHA" "$SZ" > /tmp/host/manifest.json
openssl pkeyutl -sign -inkey /tmp/sk.pem -rawin -in /tmp/host/manifest.json | base64 > /tmp/host/manifest.json.sig
# demo_manifest.json at the repo root shows this same schema as a static, unsigned
# reference — the recipe generates its own so the hash/size match the real build.

cat > /tmp/host/demo_config.yml <<'CFG'
manifest_url: http://127.0.0.1:8099/manifest.json
state_dir: /tmp/st
target: /tmp/install/demoapp
once: true
insecure: true
CFG

python3 -m http.server 8099 --directory /tmp/host &
/tmp/install/demoapp -demo /tmp/host/demo_config.yml
```

Expect `updated 1.0.0 to 9.9.9` followed by the successor process starting, no `.old`
/`.download`/`.new` left in `/tmp/install`, and no marker in `/tmp/st`.

The failure paths are the ones most worth re-checking, because they are the ones that
fail silently if broken. Flipping a byte in `a.zst` after signing must give
`hash_mismatch`; editing `manifest.json` after signing must give `signature_invalid`;
removing `manifest.json.sig` must fail rather than proceed unsigned; and setting
`insecure: false` (or omitting it) must be refused on scheme. In all four the target
binary must be left at 1.0.0.

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

`UpdateOnce` → `Checker.Check` → optional `RequireConfirmation` → `Poller.apply`, which
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

### Fail-closed points

- `TrustedVerifier()` errors on an empty trust set instead of returning a permissive
  verifier — the alternative is an RCE vector.
- `Checker.Check` refuses to touch the network with a nil `Verifier`.
- A missing `.sig` (404) is a failure, never a licence to treat the manifest as unsigned.
- Non-HTTPS URLs are rejected unless explicitly waived; the manifest and artifact
  switches are independent.
- Public keys are compile-time only (`release.go`). Never read a trust anchor from config,
  env or the network. `bakedInTrustedKeys` carries the four-step rotation procedure in
  its doc comment — the fleet must trust a new key *before* the service signs with it.

### Errors and telemetry

Every failure is tagged with an `ErrorClass` (`errors.go`) via `classify`/`classifyf`;
`ClassOf` infers one from syscall/net errors when untagged, defaulting to
`ClassInternal` so a mis-inference never fakes a tamper signal. Only the class is ever
reported upstream — raw error strings contain paths and usernames.
`signature_invalid` and `hash_mismatch` are `IsTamperSignal()` and escalate to
`SeverityAlert`. Telemetry is fire-and-forget and fully optional (nil `Reporter`).

New error paths should be classified, and telemetry `Event` fields must stay
enumerations or version strings — nothing free-form.

## Conventions

- **Comments carry the reasoning, not the mechanics.** This codebase explains *why* a
  choice was made and what breaks otherwise (see `fs_unix.go` on rename-over-running-
  executable, `update.go` on attempt accounting, `constants.go` on the space safety
  margin). Match that register; don't strip these when refactoring.
- `§N` in a comment references a numbered section of `self-update-design.md`.
- `// GB ...` comments are the repo owner's pending refactor notes, not documentation.
  Leave them unless you are doing the refactor they ask for.
- **No function exists only to forward to another.** Single-call-site wrappers were
  removed deliberately; when you inline one, its doc comment moves to the point of use
  rather than being dropped. The counter-pressure is a rough 70-line ceiling: if an
  inline would push a function past that, don't do it. `Downloader.attempt` is over the
  ceiling on purpose — it is one switch over HTTP statuses, and every way of splitting
  it recreates the helpers this layout exists to avoid.
- **Tunables live in `constants.go`**, grouped by layer, each keeping its full rationale
  comment. Enum values (`ClassNetwork`, `OutcomeSuccess`, `SeverityInfo`) stay with their
  type declarations — they are the type, not a tunable. Platform-only constants
  (`binaryMode`, `brokenSuffix`, `lockRegionLength`, `RelaunchReplacesProcess`) stay in
  the platform files.
- One platform split, `fs_unix.go` / `fs_windows.go`, with `//go:build` tags. Platform
  behaviour is injected through package vars (`execProcess`, `linkFile`) or struct fields
  (`Guard.Restore`, `Poller.Relaunch`) so the logic around it stays testable on any host.
  Those seams are worth keeping even with no tests present — they are what makes the
  Windows path reviewable from a Mac.
- Optional struct fields are zero-value-meaningful (`Interval` 0 → 1h, `MaxAttempts`
  0 → 1, nil `Logf` → discard). Follow that rather than adding constructors.
- When the suite comes back: real HTTP servers, real zstd, real Ed25519, real file swaps
  and real locks — not mocks. One behaviour per test, named as a sentence
  (`TestCheckTreatsMissingSignatureAsFailureNotAsUnsigned`). Release-side fixtures must
  not import `selfupdate`, or a fixture built with the client's own writers can agree
  with the client's own readers about an encoding the real service never emits.

## Known gaps

The Windows swap (the `fs_windows.go` rename dance with its `.broken` fallback) and the
spawn-and-exit relaunch have no coverage at all — not even the logic tests they used to
have. Their real filesystem behaviour under a running executable needs a Windows CI
runner, which does not exist here; `GOOS=windows go vet` is the whole safety net.

More broadly, nothing is verified automatically. The happy path and the four
fail-closed paths have been walked by hand on darwin/arm64 with the recipe above, which
is evidence that they worked at one point on one platform — not a regression test. See
the "no test suite" note at the top: this is the largest thing wrong with the
repository right now, and it is deliberate and temporary rather than an oversight.
