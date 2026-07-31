# Error Classes

**When:** Adding a new error path anywhere in `internal/selfupdate`, changing how an existing failure is tagged, or deciding what an unfamiliar `ErrorClass` value in a field report means.

**Source of truth:** `internal/selfupdate/errors.go`, plus the `classify`/`classifyf` call sites across the package (notably `swapError` in `fs.go`). §5 of `self-update-design.md` is the requirement. The code wins if they disagree.

---

## Why classes exist at all

Raw error strings routinely contain local file paths and usernames. §5 says telemetry
carries an error *class*, not an error string, so the class is the only failure detail
that ever leaves the machine. The full error — path, syscall, everything — stays in the
local log. See [telemetry-events.md](telemetry-events.md) for the wire side of this.

## The enumeration

`ErrorClass` is a `string` type. Ten values, unchanged since the package was split into
its current files:

| Class | Value | Produced by |
|---|---|---|
| `ClassNetwork` | `network` | `net.go` — `fetchBytes`, `Downloader.attempt`, and `Fetch`'s give-up after the last attempt; also inferred (see below) |
| `ClassManifestInvalid` | `manifest_invalid` | `release.go` — `ParseManifest`, `Manifest.Validate`, `Manifest.Artifact`; `net.go` — an over-cap response body, and `Fetch` rejecting an artifact that fails `PlatformArtifact.validate`; `update.go` — an implausible declared artifact size, an unusable published version, and a non-HTTPS artifact URL |
| `ClassSignatureInvalid` | `signature_invalid` | `release.go` — `Verifier.Verify` and `DecodeSignature` |
| `ClassHashMismatch` | `hash_mismatch` | `net.go` — the digest mismatch after a completed download, and a `Content-Length` that contradicts the signed manifest |
| `ClassDecompression` | `decompression` | `fs.go` — `DecompressFile` |
| `ClassDiskFull` | `disk_full` | `fs.go` — `ensureFreeSpace`; also inferred |
| `ClassPermissionDenied` | `permission_denied` | `fs.go` — `swapError`'s promotion, and `Relaunch`'s stat and exec failures; also inferred |
| `ClassSwapFailed` | `swap_failed` | `fs.go` — `Apply`, `RestoreOld`, `swapError`; `fs_windows.go` — the double-failure cases in the rename dance and in `restoreOld` |
| `ClassLocked` | `locked` | `fs.go` — `AcquireLock`, on the `ErrLocked` that `lockFile` in `fs_unix.go`/`fs_windows.go` returns for contention |
| `ClassInternal` | `internal` | every file, and the fallback for anything untagged |

The mapping is file-level rather than function-level on purpose: the package has been
consolidated once already (four platform pairs collapsed into `fs.go` plus two halves,
and `check.go`/`poller.go`/`rollback.go` into `update.go`), and a per-function index goes
stale faster than it earns. See [../architecture/package-selfupdate.md](../architecture/package-selfupdate.md)
for the current layout.

Note that `requireHTTPS` lives in `net.go` but does not choose its own class — the caller
passes one. `Checker.Check` passes `ClassInternal` for the manifest URL, because a
misconfigured client is a local problem, and `ClassManifestInvalid` for the artifact URL,
because that value came out of the manifest. Same function, two classes, and the
difference is which side is at fault.

## Tagging: `classify` and `classifyf`

Both return `*Error`, which carries three fields and nothing else:

```go
type Error struct {
    Class ErrorClass
    Op    string
    Err   error
}
```

`Error()` renders as `Op + ": " + Err.Error()`, and `Unwrap()` returns `Err`, so
`errors.Is` still reaches the underlying syscall or sentinel (this is how
`errors.Is(err, ErrLocked)` keeps working through the `ClassLocked` wrapper —
see [../update-cycle/locking.md](../update-cycle/locking.md), where callers depend on
telling contention apart from real failure).
`classify(class, op, err)` wraps an existing error; `classifyf(class, op, format, args...)`
builds the cause with `fmt.Errorf` when there is no wrapped error to carry.

`Op` is for the local log only. It is not a telemetry field — see
[telemetry-events.md](telemetry-events.md).

## Inference: `ClassOf`

`ClassOf(err)` is what the reporting path calls. Order of resolution:

1. `nil` → `""` (empty class, not `internal`).
2. `errors.As` finds a `*Error` anywhere in the chain → that `Error.Class`. An explicit
   tag always beats inference, at any depth.
3. `syscall.ENOSPC` / `syscall.EDQUOT` → `ClassDiskFull`.
4. `fs.ErrPermission` / `syscall.EACCES` / `syscall.EPERM` → `ClassPermissionDenied`.
5. `isNetwork(err)` → `ClassNetwork`. That covers `net.Error`, `*net.DNSError`,
   `ECONNREFUSED`, `ECONNRESET`, `EHOSTUNREACH`, `ENETUNREACH`, `io.ErrUnexpectedEOF`,
   and `context.DeadlineExceeded`.
6. Anything else → `ClassInternal`.

Inference is not a fallback the package leans on. It exists so that an `os` or `net`
error crossing an untagged boundary still reports as something legible — most notably in
`Downloader.attempt`, which hands a write failure to `classify(ClassOf(writeErr), ...)`
precisely so a full disk reports as `disk_full` rather than as the network blip it is
adjacent to.

### The default is `ClassInternal` on purpose

An untagged, unrecognized error becomes `internal` rather than anything more specific.
That is the load-bearing choice in this file: a mis-inference must never be able to
produce a tamper signal. `internal` is a class an operator reads as "we don't know yet";
`signature_invalid` is a class an operator reads as "someone is attacking the fleet."
Guessing high would poison exactly the signal that has to stay trustworthy.

### Tamper signals escalate

```go
func (c ErrorClass) IsTamperSignal() bool {
    return c == ClassSignatureInvalid || c == ClassHashMismatch
}
```

Those two classes mean the bytes on the wire did not match what the release pipeline
signed — a compromised CDN or a MITM, not a flaky hotel network. `Reporter.ReportFailure`
in `telemetry.go` escalates them to `SeverityAlert` inline; every other failure class
stays `SeverityWarn` and gets looked at in aggregate. There is no separate `severityFor`
helper — the two-line decision lives in `ReportFailure` itself, which is the only place
that needs it.

Cross-refs: §1 and §5 of the design, and
[../security/verification-order.md](../security/verification-order.md) for the
verification path that raises them,
[../security/threat-model.md](../security/threat-model.md) for what an alert means.

## Standing rules for new code

- **Every new error path gets a class.** Returning a bare `errors.New` or `fmt.Errorf` is
  not a compile error and — with no test suite in this repo, see
  [../platforms/known-gaps.md](../platforms/known-gaps.md) — not a test failure either. It
  just silently reports as `internal` and disappears into the noise. Tag it at the point
  of failure, where the reason is still known.
- **Do not tag a class you cannot prove.** If the cause is genuinely a wrapped syscall,
  let `ClassOf` infer it rather than asserting.
- **Never reach for a tamper class from a filesystem or transport path.** Only `release.go`
  (signature) and `net.go` (digest, and the `Content-Length` contradiction) may produce
  them. A `swap_failed` path that reported `hash_mismatch` because it looked more
  informative would put a fake attack on an operator's dashboard.
- **New classes are a wire-format change.** `ErrorClass` values are consumed by an
  ingestion endpoint the client does not own; adding one means the sink sees a value it has
  no rule for. Prefer an existing class unless the new one is genuinely actionable on its
  own. See [../release-contract/http-endpoints.md](../release-contract/http-endpoints.md)
  for the rest of the client's external surface.
- `swapError` in `fs.go` is the pattern to copy when a path has one interesting special
  case: it promotes permission denial to `ClassPermissionDenied` (the expected outcome of
  installing into a directory the process does not own — §6) and calls everything else
  `ClassSwapFailed`. Both platform halves route their rename failures through it, which is
  why neither `fs_unix.go` nor `fs_windows.go` needs to know the classification rules.

Related: [../update-cycle/invariants.md](../update-cycle/invariants.md) for where in the
cycle each class arises, [../update-cycle/apply-swap.md](../update-cycle/apply-swap.md)
and [../update-cycle/locking.md](../update-cycle/locking.md) for the swap and lock errors
specifically, and [../platforms/build-tags.md](../platforms/build-tags.md) for why the
platform halves stay free of classification.
