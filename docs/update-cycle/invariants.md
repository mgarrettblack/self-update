# The six invariants of the update cycle

**When:** Before reordering, inlining, extracting or short-circuiting any step in `Poller.Update` / `Poller.apply`, or before adding a new step to the cycle. Read this first if you are touching `update.go`.

**Source of truth:** `internal/selfupdate/update.go` (`Checker.Check`, `Poller.Update`, `Poller.apply`, `Guard.MarkPending`), `internal/selfupdate/fs.go` (`Apply`, `DecompressFile`), `internal/selfupdate/net.go` (`Downloader.Fetch`), `internal/selfupdate/doc.go`, `self-update-design.md` §1, §2, §3. The code wins if this document disagrees with it.

---

## The sequence

```
Update
  Checker.Check                      -> Decision
  RequireConfirmation (optional)
  apply:
    AcquireLock                      ------ held for the whole of apply
    ensureFreeSpace                  (before the first byte is requested)
    Fetch      -> <target>.download  (hash verified here)
    Decompress -> <target>.new
    Apply      -> swap onto <target>
    MarkPending                      (marker written)
    defer: remove both staging files, then release the lock
  back in Update:
    Reporter.ReportSuccess
    Reporter.Wait                    (drain)
    Relaunch
```

Each of the six invariants below is a constraint on that order. They are not
stylistic preferences — each one has a specific corruption or compromise it
prevents.

The package doc comment in `doc.go` carries the same six under the same numbers,
in one paragraph each. That list is the canonical statement; this document is the
long form, spelling out for each one what specifically breaks when it is
violated. If the two ever disagree, `doc.go` and the code are right.

The `Startup` → real startup work → `MarkHealthy` ordering is a separate
constraint, *around* the cycle rather than inside it, and lives in
[lifecycle ordering](lifecycle-ordering.md).

## 1. Verify before parse

The Ed25519 signature is checked over the exact manifest bytes as received, in
`Checker.Check`, before `ParseManifest` is called. `ParseManifest`'s own doc
comment restates the rule.

**Violated:** `encoding/json` becomes an attack surface reachable by anyone who
can answer the manifest request, and every field it produces — artifact URL, size,
digest — is attacker-controlled. The client would then fetch from a URL it has no
reason to trust. Signature verification after parsing proves nothing about the
decisions already made from the parsed document.

See [check](check.md) and [verification order](../security/verification-order.md).

## 2. Verify before decompress

SHA-256 is computed over the *compressed* bytes, streaming, inside
`Downloader.Fetch`. `DecompressFile` only ever runs on a path `Fetch` has already
returned nil for — `Poller.apply` enforces this by sequence, with the comment
"Only now, on bytes that are already verified."

The compressed bytes are the right ones to cover because they are what the signed
manifest describes and what crossed the wire (§1, §4).

**Violated:** the zstd decoder is fed unverified input, so a decompression bomb or
a decoder-level exploit is reachable before anything has been authenticated. It
also breaks the chain of custody: a digest over the *decompressed* output cannot
be compared to the manifest at all, because the manifest does not carry one.

See [decompress](decompress.md).

## 3. Never execute unverified bytes

`Apply` documents this as a precondition it does *not* itself check: "newBinary
MUST already be fully verified (hash + signature) before Apply is called."
Nothing in `fs.go` re-checks a digest. The guarantee lives entirely in the
ordering inside `Poller.apply`.

**Violated:** the live executable path is overwritten with bytes of unknown
provenance, which is remote code execution with extra steps. Because `Apply` does
not validate, a future caller that skips `Fetch` — a "re-apply the already
downloaded file" fast path, say — silently removes the whole security model. Any
new call site of `Apply` must be able to point at the verification that precedes
it.

## 4. Staging files live in the target's directory

`Poller.apply` derives both staging paths from the target itself:

```go
compressed := target + downloadSuffix   // ".download"
staged     := target + stagedSuffix     // ".new"
```

`Apply` calls `sameDirectory(newBinary, target)` and rejects a mismatch with
`ClassSwapFailed`, rather than falling back to a copy.

Same directory is the only portable way to guarantee same volume, and a
cross-volume rename is not a rename: the OS degrades it to a copy, which is not
atomic and can leave a truncated binary at the target path if the process dies
mid-write (§2). `Apply` refuses instead of copying so the caller fixes *where it
staged*, rather than inheriting a non-atomic swap it did not ask for.
`sameDirectory` compares absolute paths first and then retries with symlinks
resolved, so `/var/...` versus `/private/var/...` on macOS is not mistaken for a
volume boundary.

**Violated:** the atomicity guarantee in §2 is gone. A crash during the swap can
leave a half-written executable where the application's shortcuts, services and
supervisors expect a working one.

Note the consequence for the disk-space preflight: it must run against the
target's directory, not a temp directory elsewhere. See
[disk space](disk-space.md) and [apply and swap](apply-swap.md).

## 5. Marker after the swap, before the relaunch

`Poller.apply` calls `MarkPending` as its last statement, after `Apply` succeeded
and before `Update` relaunches.

**Written earlier:** the marker describes an update that may never happen. If the
download or the swap then fails, the next start finds a marker, counts an attempt,
and eventually reverts a binary that was never replaced — restoring a `.old` from
some previous generation, or failing outright because none exists.

**Written later:** on unix it is never written at all. `Relaunch` execs and does
not return, so any statement after it is dead code. The new binary would start
with no marker, crash-loop protection would never engage, and a bad release would
have no way back.

See [rollback](rollback.md) and [state and markers](state-and-markers.md).

## 6. Telemetry is drained before the relaunch

`Poller.Update` calls `Reporter.ReportSuccess` and then `Reporter.Wait()`
before `p.relaunch(target)`. `Poller.Startup` does the same around
`ReportRollback`.

`Report` is fire-and-forget: it spawns a goroutine and returns immediately.
`Wait` joins the in-flight ones.

**Violated:** on unix the exec replaces the process image and destroys the
in-flight goroutine mid-request, so the success event — and, on the rollback path,
the *rollback* event — is silently lost. The fleet-wide signal that matters most
(updates applied, updates reverted) would be the one signal that never arrives.

`Wait` is safe on a nil `Reporter`; telemetry is fully optional. See
[telemetry events](../observability/telemetry-events.md).

## What is not an invariant

`RequireConfirmation` is consulted after `Check` and before anything is
downloaded — consent that arrives once the bytes are already on disk is not much
of a choice — but this is a product decision, not a correctness constraint.

`Run`'s jittered interval, the download's backoff schedule and
`ReportNoUpdate` are all tunable without touching any of the six.

## Related

- [lifecycle ordering](lifecycle-ordering.md) — the ordering *around* the cycle
- [locking](locking.md) — what the lock covers and why it spans all of this
- [package internal/selfupdate](../architecture/package-selfupdate.md) — which file owns what
