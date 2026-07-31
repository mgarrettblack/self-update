# Architecture Overview

**When:** Starting any task in this repo and you do not yet know which file owns the behaviour you need to change, or you need to know how far the single-package refactor has actually got.

**Source of truth:** `go.mod`, `internal/selfupdate/doc.go` (the owner's statement of the target layering), `cmd/app/main.go`, and `self-update-design.md`. Where this doc and the code disagree, the code wins.

---

## What the module is

`self-update` (module path exactly that — no domain prefix; Go 1.26) is the **client**
half of a self-updating Go application. It fetches a signed release manifest, verifies it
against a trust set compiled into the binary, replaces its own executable atomically, and
reverts to the retained previous generation if the new build never reports healthy.

The **release** side — key generation, building, compressing, hashing, signing, hosting —
is a separate service this repository does not own and does not contain.
`self-update-design.md` is the spec; `§N` in code comments references its numbered
sections (§1 security, §2 atomicity, §3 rollback, §4 speed, §5 observability, §6 other
considerations). `README.md` documents the HTTP contract, described in
[../release-contract/http-endpoints.md](../release-contract/http-endpoints.md).

The design's phase 7 (`cmd/releaser`, `cmd/devserver`) is **not implemented**. There is no
in-repo way to produce a real signed release, and there is no test suite either (see
[Migration status](#migration-status--the-target-is-not-fully-reached)), so exercising the
client end to end requires an external release host.

## Two units

```
self-update/
├── go.mod                      module self-update; go 1.26.4
├── self-update-design.md       the spec; §N references point at its sections
├── README.md                   the HTTP contract the client expects
├── cmd/app/main.go             demo integration — documents the call ordering
└── internal/selfupdate/        the whole updater library, one package
    ├── doc.go                  package doc: the target layering, three invariants
    ├── errors.go               ErrorClass, Error, classify, ClassOf
    ├── fs.go                   the OS: lock, space, swap, relaunch, decompress, state
    ├── fs_unix.go              flock, statfs, rename-over-running-exe, execve
    ├── fs_windows.go           LockFileEx, GetDiskFreeSpaceEx, rename dance, spawn
    ├── net.go                  manifest/signature fetch, resumable artifact download
    ├── release.go              semver, manifest, signatures, trust set, rollout
    ├── update.go               Checker, Guard, Poller — the orchestration layer
    ├── telemetry.go            Reporter, Event — cross-cutting, optional
    └── rollback.md             in-package rationale for crash-loop detection
```

| Unit | Role | Read next |
| --- | --- | --- |
| `internal/selfupdate` | Everything: check, download, verify, decompress, swap, mark, relaunch, roll back, report. Depends only on `zstd`, `x/sys` and the standard library. | [package-selfupdate.md](package-selfupdate.md) |
| `cmd/app` | A demo application whose reason for existing is to document the required call ordering. Not a product. | [cmd-app.md](cmd-app.md) |

There used to be an `internal/version` package and a test-only `internal/releasetest`
package. Both are gone. The semver code moved into `release.go` — see
[versioning.md](versioning.md), which also covers the `0.0.0-dev` trap. `releasetest`
existed only to feed the tests, and nothing of it survives.

## The layering: distance from the OS

`doc.go` organises the package by how far each file sits from the operating system.
Dependencies run strictly **downward** through this list; nothing below `update.go` calls
back up into it.

```
                       cmd/app
                          │
          ┌───────────────▼──────────────────────────────┐
          │  update.go    Checker · Guard · Poller       │
          │  the only file that calls net, release and   │
          │  fs together — the ordering lives here       │
          └───┬───────────────┬───────────────┬──────────┘
              ▼               ▼               ▼
        release.go         net.go           fs.go (+ fs_unix / fs_windows)
        semver, manifest,  manifest+sig     lock, space, swap, relaunch,
        signatures, trust  fetch, resumable decompress, install id,
        set, rollout       artifact download state dir
              └───────────────┴───────────────┘
                              ▼
                   errors.go  ·  constants.go
                      (depend on nothing)

        telemetry.go — cross-cutting, callable from anywhere
```

Why this shape rather than files named after features: the update cycle is one sequence
whose steps are individually uninteresting and jointly load-bearing. Grouping by distance
from the OS means a change to *how* something is stored or fetched touches one layer, while
the ordering that makes the whole thing safe lives in exactly one file where it can be read
top to bottom. The rules, and what breaks when they are violated, are in
[dependency-rules.md](dependency-rules.md).

## Migration status — the target is not fully reached

`doc.go` describes the target. The tree now matches it almost exactly: the old
per-feature files (`check.go`, `poller.go`, `rollback.go`, `lock.go`, `space.go`,
`apply.go`, `compress.go`, `state.go`, `relaunch.go`, `download.go`, `keys.go`,
`manifest.go`, `rollout.go`, `verify.go`, and every `_unix`/`_windows` half of those) are
**all gone**, and `go build ./...` and `go vet ./...` are clean. Two gaps remain:

- **`constants.go` does not exist.** The constants `doc.go` places at the bottom of the
  layering are still declared in the files that use them: poll interval, jitter, staging
  suffixes, the manifest/signature size caps and the decompression ratio estimate in
  `update.go`; fetch attempts, backoff and buffer size in `net.go`; the space safety
  margin, the `.old` suffix and the 1 GiB decompression cap in `fs.go`. Nothing is broken
  by this — it is a file that has not been split out yet. Each constant still carries the
  comment explaining its value, which is the part that matters.
- **`doc.go` names `Guard.Startup` and `Guard.MarkHealthy`** as the pair an integrator
  calls. On disk the integrator-facing methods are **`Poller.Startup`** and
  **`Poller.MarkHealthy`**; `Guard` exposes `CheckStartup` / `MarkPending` /
  `MarkHealthy`, which `Poller` delegates to. There is no `Guard.Startup` at all. Trust
  the code: write `Poller.Startup`.

**There are no tests.** All 18 `*_test.go` files were deleted along with
`internal/releasetest`. `go test ./...` compiles and runs nothing. Any claim elsewhere in
these docs about the test suite, or any citation of a test by name, is stale — treat
behaviour as verified only by reading the code.

## The two things most likely to bite you

1. **Call ordering.** `Poller.Startup()` → the app's real startup → `Poller.MarkHealthy()`
   → `go Poller.Run(ctx)`. `MarkHealthy` is what deletes the crash-loop marker and the
   retained `.old` binary, so calling it before the startup work that can fail defeats
   rollback entirely. See [cmd-app.md](cmd-app.md) and
   [../update-cycle/lifecycle-ordering.md](../update-cycle/lifecycle-ordering.md).
2. **Verification order.** Signature over the raw manifest bytes before any JSON parsing;
   SHA-256 over the *compressed* artifact before decompression; never execute unverified
   bytes. See [../security/verification-order.md](../security/verification-order.md) and
   [../update-cycle/invariants.md](../update-cycle/invariants.md).

## Commands

```sh
go build ./...   # both ldflags matter for a usable binary — see dependency-rules.md
go vet ./...
```
