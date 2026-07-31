# Verification Order

**When:** Read this before editing `Checker.Check` or `Poller.apply` in `internal/selfupdate/update.go`, `fetchBytes`/`Downloader.Fetch` in `net.go`, or `DecompressFile`/`Apply` in `fs.go` — i.e. before touching anything on the path from "fetch the manifest" to "swap the binary". Also read it before adding a step to that path.

**Source of truth:** `internal/selfupdate/update.go` (`Checker.Check`, `Poller.apply`), `internal/selfupdate/net.go` (`fetchBytes`, `Downloader.Fetch`, `attempt`), `internal/selfupdate/release.go` (`DecodeSignature`, `Verifier.Verify`, `ParseManifest`), `internal/selfupdate/fs.go` (`DecompressFile`, `Apply`), `self-update-design.md` §1. If this doc and the code disagree, the code wins.

---

The sequence below is fixed. Each step exists to make the *next* step safe, so
reordering one does not weaken the chain slightly — it removes a link.

```
fetch manifest bytes ─┐
fetch .sig bytes    ─┤→ Verify(rawBytes, sig) → ParseManifest → decide
                                                                 │
                       Fetch (SHA-256 over compressed bytes) ←───┘
                                    │
                       DecompressFile ← only on verified bytes
                                    │
                       Apply (rename onto target)
                                    │
                       MarkPending → Relaunch (first execution)
```

## 1. Signature over the raw manifest bytes, before any parsing

`Checker.Check` fetches the manifest body, fetches `<manifest-url>.sig`,
`DecodeSignature`s it, calls `c.Verifier.Verify(body, sig)` — and only then calls
`ParseManifest(body)`. The comment marking the boundary reads *"Trusted from here on,
and only from here on."*

The signature covers the bytes exactly as received. Nothing normalises, re-encodes or
re-serialises the manifest first, and nothing may start doing so: verify-then-transform
is fine, transform-then-verify is a signature over bytes nobody signed.

**Skip it, or move `ParseManifest` above it, and the attacker gets:** every field of the
manifest under their control, plus a JSON parser reachable with arbitrary bytes. The
manifest is a list of URLs and hashes the client will download and then *execute* — an
attacker-authored manifest is an attacker-chosen binary. Parsing first also hands
untrusted input to `encoding/json` and to `parseSemver`, which is a needless attack
surface for zero benefit: nothing learned from an unverified manifest may be acted on
anyway.

Two supporting details in `fetchBytes` protect this step. It is one function in `net.go`
serving both requests, so neither can drift from the other:

- The body is read through `io.LimitReader(resp.Body, max+1)` and rejected if it
  exceeds `max`. `Check` passes `defaultMaxManifestBytes` (1 MiB, overridable via
  `Checker.MaxManifestBytes`) for the manifest and `maxSignatureBytes` (4 KiB, fixed)
  for the signature. Reading one byte past the cap means oversize is *detected* rather
  than silently truncated — truncation would change the bytes the signature covers and
  surface as a confusing signature failure instead of the size failure it is.
- A non-200 status is an error, including 404. See
  [fail-closed-points.md](fail-closed-points.md) on why a missing `.sig` can never be
  read as "unsigned".

The signature URL is *derived* (`signatureURLSuffix = ".sig"`), never configured
separately, so no misconfiguration can pair release A's manifest with release B's
signature.

`DecodeSignature` bounds the other end: standard base64, whitespace trimmed, and a
decode that must land on exactly `ed25519.SignatureSize` bytes or it is
`ClassSignatureInvalid` before `Verify` is ever called. See
[../release-contract/signature-format.md](../release-contract/signature-format.md).

## 2. SHA-256 over the compressed artifact, before any decompression

`Downloader.Fetch` streams the artifact to `<target>.download`, hashing in the same
pass, and returns nil only if the digest equals `art.SHA256` from the signed manifest.
`Poller.apply` then calls `DecompressFile` — the comment there is *"Only now, on bytes
that are already verified."*

The digest is over the **compressed** bytes: the ones that actually crossed the wire and
the ones the signed manifest describes (`PlatformArtifact.SHA256`/`Size` document the
compressed artifact). Hashing after decompression would mean the signed value no longer
describes anything the client can check before running a decompressor on hostile input.

**Skip it, or decompress first, and the attacker gets:** the zstd decoder as an attack
surface, reachable with arbitrary bytes, on every client in the fleet — and a
decompression bomb that fills the user's disk. `fs.go` is explicit that
`maxDecompressedBytes` (1 GiB) is *not* the primary defence here; it is a backstop that
turns "a bad release fills every disk" into a clean `ClassDecompression` failure,
because the hash check already means a bomb would have to have come from the release
service itself.

Supporting details that keep the hashed bytes the *published* bytes:

- `Accept-Encoding: identity` on the artifact request. Left alone, the transport
  advertises gzip and transparently decodes, so the bytes hashed would not be the bytes
  signed, and `Content-Length` would describe neither. Setting the header also disables
  automatic decoding, so a server that compresses anyway fails the digest rather than
  slipping through. Note this is set only on the artifact request in
  `Downloader.attempt`; `fetchBytes` sets no `Accept-Encoding` at all, which is safe for
  a different reason — see
  [../release-contract/http-endpoints.md](../release-contract/http-endpoints.md).
- The body is read through `io.LimitReader(resp.Body, remaining)`, so a server cannot
  stream forever while claiming a few megabytes.
- A `Content-Length` that contradicts the manifest is `ClassHashMismatch` and is **not**
  retried — a content disagreement cannot be fixed by asking again.
- Resume is only trusted when `Content-Range`'s start equals the on-disk length;
  anything ambiguous restarts from zero, because appending to a mismatched prefix
  produces a file that matches no digest.
- On a hash or size mismatch the partial is **deleted**. A prefix that disagrees with
  the manifest is not worth resuming, and leaving it would poison every later attempt.

## 3. Only then the swap

`Apply(staged, target)` is the first thing that touches the live executable path, and it
documents verification as a **precondition it does not itself check**:

> `newBinary` MUST already be fully verified (hash + signature) before Apply is called.

`Apply` cannot re-verify — by that point the bytes are decompressed and the signed
digest no longer describes them — so the guarantee lives entirely in the ordering of
`Poller.apply`. That is why the three calls sit in one small function with the
verification comments attached.

`Apply` does enforce one thing: `newBinary` must be in the same directory as `target`,
or it fails with `ClassSwapFailed`. This is an atomicity requirement rather than an
authenticity one — a cross-volume rename silently degrades to a copy, which can leave a
truncated binary at the target path — but it is on this path, so a change that stages
downloads into `os.TempDir()` will break it. See
[../update-cycle/apply-swap.md](../update-cycle/apply-swap.md).

**Skip the ordering and the attacker gets:** the live binary replaced with unverified
content. There is no recovery from that; the next launch executes it.

## 4. The binary is never executed before it is verified

Nothing in this package runs a downloaded file to test it. §1 states the rule as *never
execute-then-verify*, and the only execution of new bytes is `Relaunch` on the target
path *after* `Apply` has already put verified content there.

`Poller.apply` writes the crash-loop marker between the swap and the relaunch —
`MarkPending` after `Apply`, and the relaunch does not happen until `UpdateOnce` calls
it. Both edges are load-bearing: earlier fires a revert for an update that never
happened, later never runs at all, because on unix `Relaunch` replaces the process image
and does not return. Telemetry is drained (`Reporter.Wait`) before the relaunch for the
same reason — that drain, and the relaunch itself, live in `UpdateOnce` rather than
`apply`, because `apply` returns while the lock's deferred cleanup still has to run.
Neither is a verification step, but both live inside this sequence and cannot be moved
out of it. See [../update-cycle/rollback.md](../update-cycle/rollback.md) and
[../update-cycle/lifecycle-ordering.md](../update-cycle/lifecycle-ordering.md).

## Invariants to preserve

1. Nothing untrusted reaches a parser: verify, then `ParseManifest`.
2. Nothing unverified reaches a decompressor: `Fetch`, then `DecompressFile`.
3. Nothing unverified reaches the target path: decompress, then `Apply`.
4. No failure anywhere in the chain has a permissive branch. Every one deletes its
   staging files (`Poller.apply`'s deferred cleanup runs before the lock is released)
   and returns a classified error; the next cycle starts clean.

There is no test suite backing any of this — `internal/releasetest` and the `_test.go`
files are gone — so these four lines are the whole regression net. A change to the
sequence has to be argued from the code.

Related: [trust-model.md](trust-model.md), [fail-closed-points.md](fail-closed-points.md),
[../update-cycle/invariants.md](../update-cycle/invariants.md),
[../update-cycle/download-and-resume.md](../update-cycle/download-and-resume.md).
