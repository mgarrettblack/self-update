# Threat Model

**When:** When asked what this updater defends against, when evaluating whether a proposed change (a new input, a new fallback, a new install location) weakens a guarantee, or when triaging a `signature_invalid` / `hash_mismatch` report from the field.

**Source of truth:** `self-update-design.md` §1 and §6, `internal/selfupdate/release.go` (trust set, verifier, manifest, rollout), `update.go` (`Checker.Check`, `Poller`, `Guard`), `net.go` (fetch and download), `fs.go` (lock, swap, state dir, install id), `telemetry.go`, `README.md`. If this doc and the code disagree, the code wins.

---

## What is inside the boundary

One asset: **the bytes this process will execute after it updates itself.** Everything
else here serves that. The trust anchor is the compile-time set of Ed25519 public keys
([trust-model.md](trust-model.md)); the enforcement is the fixed verification sequence
([verification-order.md](verification-order.md)).

## Defended: a compromised CDN, bucket, or MITM

This is the primary adversary. Assume an attacker who can serve arbitrary responses for
the manifest, the `.sig`, and the artifact URLs — a poisoned object store, a hijacked
CDN edge, a TLS-terminating proxy, a stolen hosting credential.

They cannot get code executed. The manifest signature is checked over the raw bytes
before parsing, and the artifact digest comes from that signed manifest and is checked
over the compressed bytes before decompression. Without a trusted private key the
attacker can produce neither. Specifically:

- **Swapped artifact** → digest mismatch (`ClassHashMismatch`); the partial is deleted
  and the swap never runs.
- **Edited manifest** (new URL, new digest, higher version) → `ClassSignatureInvalid`
  before `ParseManifest` sees a byte of it.
- **Deleted `.sig`** → non-200 is a failure, never "this release is unsigned".
- **Transport re-encoding** (a proxy or CDN gzipping the artifact) → the client sends
  `Accept-Encoding: identity`, so a server that compresses anyway fails the digest
  instead of slipping through.
- **Endless body / bomb** → responses are capped (`LimitReader` on manifest, signature
  and artifact body) and decompression is bounded at 1 GiB.

Both tamper classes are `IsTamperSignal()` and report at `SeverityAlert`, so this
adversary is loud rather than silent — that separation exists because a hash mismatch is
"a compromised CDN or a MITM, not a flaky hotel network".

HTTPS is still required (`requireHTTPS`, waivable only per-URL for local testing). §1 is
explicit that transport security is *necessary but not sufficient*, and the reverse is
also true — which is the next item.

## Partially defended: suppression and replay

Signatures prove authenticity, not freshness. This design has no manifest expiry, no
nonce, no monotonic counter the client remembers, and no signed timestamp. So an attacker
positioned to control responses can **stop a client from updating** even though they
cannot make it install anything of theirs:

- Serve an older, still-authentically-signed manifest. `IsNewer` returns false and the
  client reports "no update" as an entirely normal outcome. A pending security fix never
  arrives, and nothing looks wrong locally.
- Simply fail or black-hole the requests. `Poller.Run` logs and retries next tick — by
  design, since an unreachable release host must not take the application down.

This is why **signatures alone do not make plaintext HTTP safe**, and why `requireHTTPS`
exists as a separate control rather than being considered redundant. Over HTTPS an
off-path attacker cannot select which signed manifest the client sees; over plaintext
anyone on the path can, indefinitely and undetectably. The `insecure` config waiver is for
local testing against a dev release host, nothing else.

Partial mitigations that exist: `Cache-Control: no-cache` on the manifest and signature
requests (aimed at CDNs serving a pulled release, not at an adversary), and the fact that
suppression is visible *centrally* — the version census in telemetry shows a cohort that
stops advancing. Detecting suppression is an operator-side job; the client cannot
distinguish "no new release" from "no new release being shown to me".

## Out of scope: a malicious or compromised release service

The release service holds the private key. If it is compromised — or if someone with
access to it signs a malicious release — every client that trusts that key will accept
the result, verify it correctly, and run it. That is not a gap in the client; it is the
definition of the trust relationship.

Consequences worth being explicit about:

- Nothing on the client detects a "bad but authentic" release. A decompression bomb, a
  binary that wipes files, a downgrade to a version with a known CVE (a *newer* version
  containing old code) all pass verification.
- The controls that limit damage are blast-radius controls, not security controls:
  staged rollout (`rollout` percentage, so a bad release reaches 10% first) and
  crash-loop rollback (`.old` retained one generation, marker cleared only by
  `MarkHealthy`). Both are about *bad releases*, not *hostile* ones — a hostile release
  would simply not crash. See
  [../update-cycle/rollout-cohorts.md](../update-cycle/rollout-cohorts.md)
  and [../update-cycle/rollback.md](../update-cycle/rollback.md).
- Containment for a compromised key is rotation, which cannot outpace fleet update rates
  and does not revoke until step 4 completes. See
  [key-management-and-rotation.md](key-management-and-rotation.md).
- Key custody, offline storage, and CI hardening belong to the service and are not
  addressed in this repository, which contains no signing code at all.

## Out of scope: a local attacker with write access

If an attacker can write to the target directory, they can replace the binary directly
and never involve the updater. Nothing here defends against that, and nothing could:

- **Target directory.** The client does not choose where it is installed and provides no
  install-location helper: `Poller.target` replaces the running executable, resolved
  through `os.Executable()` and `filepath.EvalSymlinks` (or whatever `TargetPath` /
  `cmd/app`'s `target` config field names). The design's §6 guidance still governs the *packaging*
  decision — prefer a per-user location such as `~/.local/bin` or
  `%LOCALAPPDATA%\Programs\<app>`, because a process can only replace its own executable
  if it owns the directory — and that trades a system-wide install's protection-by-root
  for the ability to self-update without elevation. Anyone who is that user, or is root,
  already owns the application. A directory the process cannot write reports
  `ClassPermissionDenied` rather than failing obscurely.
- **State directory.** The lock, `install-id` and `update-pending.json` live in a
  per-user state dir (`DefaultStateDir`: `~/.local/state/<app>`, or
  `%LOCALAPPDATA%\<app>\state`) created `0o700`, with the marker and install ID written
  `0o600`. The lock file itself is `0o644` — its contents are never read or written, only
  its existence and lock state matter. Tampering there costs an unwanted rollback, a
  reshuffled rollout cohort, or a skipped swap — annoying, not an execution primitive. The
  marker is parsed only for two version strings and an attempt count, and a corrupt one
  reverts rather than being trusted.
- **Staging files.** `<target>.download` (`0o600`) and `<target>.new` are removed on
  every exit path, and both are re-created under a `flock` held for the whole cycle. A
  local attacker who can write into that directory can influence the swap — but again,
  they can write into the directory holding the executable.
- **The lock is advisory.** `fs.go` says so outright: it coordinates cooperating copies
  of this program and *"is not a defense against a hostile local process"*.

## Not addressed here

- **OS code signing** (Apple notarization, Windows Authenticode) is a complementary
  layer, per §1: it governs whether the OS will execute the binary; Ed25519 governs
  whether the updater will install it. Both are needed and this repo implements only the
  second. Reputation lag on freshly signed Windows binaries (§6) can look like an update
  failure and is not something to engineer around.
- **Privacy.** A random 16-byte `install-id` is generated locally and attached to every
  telemetry event — deliberately random rather than derived from hostname or username.
  Only enumerations and version strings are ever sent; raw error strings, which contain
  paths and usernames, stay in the local log. Telemetry is off entirely with a nil
  `Reporter`.
- **Denial of service against the release host.** Poll jitter and download backoff jitter
  exist to prevent a self-inflicted thundering herd, not to resist an attacker.
- **Assurance.** Every claim above is a property of the code as read, not a tested one:
  there is no test suite in this repository, and no in-repo way to produce a signed
  release to exercise the happy path against. The Windows swap and relaunch are the
  sharpest edge of that, since they have no coverage *and* no CI runner; see
  [../platforms/windows.md](../platforms/windows.md) and
  [../platforms/known-gaps.md](../platforms/known-gaps.md).
