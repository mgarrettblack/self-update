# Fail-Closed Points

**When:** Before writing a new error path in `internal/selfupdate`, before adding a fallback/"best effort"/"probably fine" branch anywhere in the update cycle, or when reviewing a change that makes a failure non-fatal. Also the checklist to consult when asked "why does this refuse instead of degrading?"

**Source of truth:** `internal/selfupdate/release.go` (trust set, verifier, manifest validation, semver), `update.go` (`Checker.Check`, `Poller.apply`, `Guard`), `net.go` (`fetchBytes`, `Downloader`), `fs.go` (lock, space, swap, decompress, state), `errors.go`. If this doc and the code disagree, the code wins.

---

Each entry is a place the code chooses failure over permissiveness, and what would go
wrong if it chose otherwise. New error paths should extend this list, not shorten it.
There is no test suite; this list is the checklist that replaces it.

## Trust and verification

**`TrustedVerifier()` errors on an empty trust set.** It merges `bakedInTrustedKeys` with
the `-ldflags` list, trims, drops blanks and duplicates, and then — if nothing survived —
returns a `ClassInternal` error naming the `-ldflags` invocation rather than a verifier.
The check is on the *merged* result, which is what makes it robust: neither source is
individually required, `bakedInTrustedKeys` is empty as committed in this repository, and
a blank `TrustedKeysBase64` deliberately contributes zero entries rather than one empty
string. A build with no keys would otherwise have to choose at runtime between "reject
everything" and "accept anything", and the second is a remote code execution vector for
anyone able to answer an HTTP request. Failing at construction makes the mistake visible
on the first check instead of in an incident.

**`NewVerifier` rejects an empty key list, and any key of the wrong length.** The empty
case is rejected even though a verifier over zero keys would reject every signature
"safely": it would silently disable updates forever rather than fail visibly, so a fleet
would quietly stop receiving security fixes. A wrong-length key is `ClassInternal` at
construction, not a signature comparison that mysteriously never matches.

**`Verify` has no partial success.** Wrong signature length and no-matching-key are both
`ClassSignatureInvalid`. There is no fallback path that accepts unverified bytes.

**`DecodeSignature` rejects a signature of any length but 64.** A `.sig` that decodes to
something else is `ClassSignatureInvalid` before `Verify` runs, so a truncated or padded
signature file cannot reach the comparison at all.

**`Checker.Check` refuses the network with a nil `Verifier`.** The guard is the first
statement in the function, before the URL checks and before any request: *"a client with
no trust set must not fetch a manifest it has no way to verify, because the only thing
it could do with the result is act on unverified data."* Fetching first would waste the
request and, worse, invite a later patch to "just read the version out of it".

**A missing `.sig` is a failure, never a licence to treat the manifest as unsigned.**
`fetchBytes` rejects any status other than 200, and the comment calls out 404
specifically. The alternative — "no signature file, so this release must be unsigned" —
turns deleting one object from a bucket into a complete bypass of the trust model.

**Non-HTTPS is rejected unless explicitly waived, and the two waivers are independent.**
`requireHTTPS` is called on the manifest URL with `AllowInsecureManifestURL` (before any
request) and on the artifact URL with `AllowInsecureArtifactURL` (after the manifest is
verified, before the download). Keeping them separate lets a dev setup serve a manifest
from `http://localhost` while still enforcing real policy on the bytes that will be
executed. Note that `cmd/app`'s single `insecure` config field sets both — that is the demo
app's choice, not the library's. Plaintext is not made safe by signatures: an attacker
who can rewrite responses can replay a stale-but-authentic manifest and suppress updates
indefinitely (see [threat-model.md](threat-model.md)).

**The manifest is validated after verification anyway.** `Manifest.Validate` re-checks
semver, rollout range, non-empty platforms, 64-char lowercase hex digests, positive size
and absolute URLs, because *"a signature proves the manifest came from the release
pipeline, not that the pipeline got it right."*

**Oversize responses are detected, not truncated.** Both `fetchBytes` calls read
`LimitReader(body, max+1)` and error above `max`. Silent truncation would change the
bytes the signature covers and surface as a confusing signature failure instead of the
size problem it is.

**Only strictly newer versions install.** `IsNewer` gates the update, so a manifest
advertising an older release — including an authentically signed rollback — moves nobody
backwards. An invalid running version is an error before the network, since a client that
cannot compare itself to a manifest cannot make the decision at all; it is
`ClassInternal`, because it means the build forgot its `-X ...selfupdate.Version=` flag.

## Download, decompress, swap

**`Downloader.Fetch` re-runs `art.validate()` before making a request**, so a direct
caller cannot bypass the manifest's own rules with a hand-built `PlatformArtifact`.

**A `Content-Length` that contradicts the manifest is `ClassHashMismatch` and is not
retried**, and the partial is deleted. It is a content disagreement, not a transient
blip; retrying cannot change the answer, and whatever is on disk is not a prefix of the
real artifact. Same for a hash mismatch at the end: `os.Remove(destPath)`, because
leaving it would poison every subsequent attempt forever.

**Ambiguous resume state restarts from zero rather than guessing.** A 200 answer to a
ranged request, a missing or disagreeing `Content-Range`, a partial whose hash cannot be
re-seeded, or a file already at/past the advertised size — each resets to offset 0.
Appending to a mismatched prefix would silently produce a file matching no digest.

**`DecompressFile` removes `dst` on every failure path**, including a zero-byte result
and an overrun of `maxDecompressedBytes`, so a half-written file can never be mistaken
for a complete binary by a later swap.

**`Apply` rejects a cross-directory source instead of falling back to a copy.** A
cross-volume rename is not atomic — the OS degrades it to a copy that can leave a
truncated binary at the target path. It fails with `ClassSwapFailed` so the caller fixes
where it staged, rather than papering over it.

**An implausible declared size is refused before the preflight.** `Poller.apply` computes
`size * (1 + decompressionRatioEstimate)` and checks the product against the original: if
it went backwards, the multiplication overflowed, and the manifest is rejected as
`ClassManifestInvalid` rather than the preflight being handed a negative requirement.

**`ensureFreeSpace` treats a failed `statfs` as an error, not as "probably fine."**
Proceeding on an unknown amount of space defeats the point of a preflight whose entire
job is preventing a mid-write ENOSPC. It also refuses a non-positive requirement as
`ClassInternal` rather than `ClassDiskFull`, so a bad `size` field cannot be mistaken in
telemetry for a fleet of full disks. Unix uses `Bavail`, not `Bfree`, because
root-reserved blocks are not space this process has.

**A corrupt crash-loop marker reverts.** `Guard.CheckStartup` does not read an
unparseable marker as "no update pending": the file only exists because an update was
applied and never confirmed healthy, and a half-written marker is itself a plausible
symptom of the crash being looked for. With the attempt count unrecoverable, the
conservative reading is "an attempt was made and failed".

**Lock contention skips the swap rather than racing.** `AcquireLock` is non-blocking and
`ErrLocked` (`ClassLocked`) is distinguishable from real failures, so a second instance
does nothing and picks the new binary up on its next restart. `Release` deliberately does
*not* unlink the lock file — a fresh inode with the same name would let two instances
each hold "the" lock.

**`Relaunch` stats the target before handing over.** On unix the exec is a point of no
return, so a missing file, a directory, or a permission problem is reported as a legible
classified error while that is still possible, rather than as an unrecoverable failure
after the process image is gone. A nil `execProcess` is `ClassInternal`, not a silent
no-op.

## Classification and telemetry

**`ClassOf` defaults to `ClassInternal`.** An untagged, unrecognised error becomes
`internal` rather than anything more specific, *"so a mis-inference never masquerades as
a tamper signal."* `ClassSignatureInvalid` and `ClassHashMismatch` are
`IsTamperSignal()` and escalate to `SeverityAlert`; if inference could reach them by
accident, the one alert meant to indicate a compromised CDN would become noise.

**`Event` carries only enumerations and version strings.** Raw error text routinely
contains local paths and usernames, so only the class leaves the machine.

## Deliberately *not* fail-closed

These are availability decisions, and the asymmetry is intentional: refusing to verify
is cheap, refusing to *run the application* is not.

- **A failed update check never propagates.** `Poller.Run` logs and waits for the next
  tick. An unreachable release host must not take down the application the updater
  exists to maintain (§6).
- **An unwritable state dir does not block startup.** `CheckStartup` proceeds when the
  incremented attempt count cannot be persisted, rather than wedging the app over a
  bookkeeping file.
- **A failed relaunch continues on the current process.** The swap already succeeded and
  the marker is in place; staying alive on the old image beats exiting, and the next
  start picks up the new binary.
- **Telemetry failures are swallowed entirely**, including a `recover()` around the send
  goroutine. Telemetry must never affect the update path.
- **An omitted `rollout` field means 100, not 0.** JSON's zero value would mean "ship to
  nobody" — a silent, invisible failure — so the permissive reading is the *visible*
  one. Rollout is a blast-radius control, not a security boundary.
- **A release with no artifact for this platform is "no update", not an error.**
  `Check` catches `Manifest.Artifact`'s error and turns it into a `Reason`, so a
  staggered platform rollout does not fill logs with failures on the platforms that are
  not out yet. An entirely empty `platforms` map is still `ClassManifestInvalid`.
- **The update lock is advisory.** It coordinates cooperating copies of this program and
  is not a defence against a hostile local process; see
  [threat-model.md](threat-model.md).

Related: [verification-order.md](verification-order.md), [trust-model.md](trust-model.md),
[../observability/error-classes.md](../observability/error-classes.md),
[../update-cycle/invariants.md](../update-cycle/invariants.md).
