# Detached Signature Format

**When:** Debugging a `signature_invalid` failure, changing how the manifest is fetched, decoded or serialized, or wiring up a service that signs manifests.

**Source of truth:** [`internal/selfupdate/release.go`](../../internal/selfupdate/release.go) (`DecodeSignature`, `Verifier.Verify`, the trust set), [`internal/selfupdate/update.go`](../../internal/selfupdate/update.go) (`Checker.Check`, the fetch-then-verify order), [`internal/selfupdate/net.go`](../../internal/selfupdate/net.go) (`fetchBytes`), design §1. Where this file and the code disagree, the code wins.

---

Signing belongs to the release service, which **this repository does not
contain**: no private key ever exists in this codebase, and there is no in-repo
tool that signs a manifest (`cmd/releaser`, design phase 7, is unimplemented).
There is no reference producer here at all any more — the test-only
`internal/releasetest` package that used to fabricate real Ed25519 signatures has
been removed along with the rest of the test suite, so **nothing in this
repository can generate or verify a signature end to end**. The format below is
the client's decoder read literally; a service implementing it has to be checked
against a real client build.

## Wire format

The file at `<manifest-url>.sig` (URL derived, not configured — see
[http-endpoints.md](http-endpoints.md)) contains:

- **one** Ed25519 signature, 64 raw bytes (`ed25519.SignatureSize`),
- encoded with **standard** base64 (`base64.StdEncoding` — `+`/`/` alphabet, `=`
  padding; *not* URL-safe base64),
- optionally with surrounding whitespace, because most tooling appends a trailing
  newline. `DecodeSignature` applies `strings.TrimSpace` to the whole file
  contents before decoding, so leading whitespace and a trailing `\n` or `\r\n`
  are all tolerated. Interior whitespace is not: a wrapped, multi-line base64
  block fails to decode.

Nothing else is in the file: no PEM armour, no headers, no key ID, no second
signature. `DecodeSignature` rejects anything that does not base64-decode
(`ClassSignatureInvalid`) or that decodes to a length other than 64
(`ClassSignatureInvalid`, reporting the length it got). `Verifier.Verify`
independently re-checks the length before touching a key, so a wrong-length
signature can never reach `ed25519.Verify`. The response body is capped at
`maxSignatureBytes` = 4 KiB, so a signature file padded with junk fails as an
oversized body (`ClassManifestInvalid`) rather than as a signature problem.

## What the signature covers

The **raw manifest bytes exactly as served** — the response body of
`GET <manifest-url>`, unmodified, un-normalised, unparsed. One signature covers
the whole document, including every platform's URL and digest, so there is no
per-artifact signature to get out of step.

This is why byte-for-byte fidelity is absolute on the service side. Ed25519 signs
a byte string, not a JSON object, so any re-serialization between signing and
serving breaks verification permanently for every client:

- reformatting, re-indenting, or minifying the JSON,
- reordering keys (Go's `encoding/json` sorts map keys, so a round-trip through
  `map[string]any` almost certainly reorders),
- adding or stripping a trailing newline,
- transcoding, BOM insertion, or CRLF conversion,
- any middlebox that "helpfully" rewrites the body.

The service must publish the exact bytes it signed and sign the exact bytes it
publishes. If a manifest must change, sign the new one — do not edit a published
manifest in place.

The client's own read path is built to preserve those bytes. `fetchBytes` reads
`LimitReader(body, max+1)` and rejects an oversized body rather than truncating
it, because a truncated read changes what the signature covers and would surface
as a baffling signature failure rather than a size failure. Note the one place
the client does *not* pin the encoding: `fetchBytes` sets no `Accept-Encoding`, so
Go's transport may negotiate gzip and transparently decode, and what is verified
is the body after that decoding. That is safe because the signature is over the
document, not over the stored octets — unlike the artifact, whose digest is over
the compressed bytes and whose request therefore pins
`Accept-Encoding: identity`.

## Verification order

From `Checker.Check`, and this order is load-bearing:

1. `fetchBytes` the manifest body.
2. `fetchBytes` the `.sig` body.
3. `DecodeSignature`.
4. `Verifier.Verify(body, sig)`.
5. **Only now** `ParseManifest(body)`.

Nothing untrusted reaches a parser. An attacker who controls the CDN cannot even
get the client's JSON decoder to run on their bytes, let alone get it to act on a
`url` field. Nothing is downloaded before step 4 succeeds. Full treatment in
[`../security/verification-order.md`](../security/verification-order.md).

## Failure is always closed

- `Verifier.Verify` tries every key in the trust set and returns
  `ClassSignatureInvalid` if none matches — the message reports how many keys were
  in the set, never which. There is no partial success and no fallback path that
  accepts unverified bytes.
- **A `404` on the `.sig` is a hard failure.** A missing signature file is never a
  licence to treat the manifest as unsigned — that is the single most valuable bug
  an attacker could hope for, since suppressing one file is far easier than forging
  a signature. Note that this surfaces as `ClassNetwork` ("unexpected status"),
  not `signature_invalid`, because it is the *fetch* that failed.
- A nil `Verifier` makes `Check` refuse to touch the network at all.
- `TrustedVerifier()` errors on an empty trust set instead of returning a
  permissive verifier — the alternative is an RCE vector.

`signature_invalid` is an `IsTamperSignal()` class and escalates to
`SeverityAlert` in telemetry: bytes on the wire that do not match what was signed
mean a compromised CDN or a MITM, not a flaky hotel network. See
[`../observability/error-classes.md`](../observability/error-classes.md).

## Keys, briefly

The trust set is a *set* of Ed25519 public keys compiled in at link time
(`TrustedKeysBase64`, standard base64, comma-separated, injected with
`-X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY`), merged with the
`bakedInTrustedKeys` slice committed to source — which is **empty** in this
repository, so a build without the flag has no trust set and its updater refuses
to start. Public keys are never read from config, env or the network. Rotation
requires the fleet to trust a new key *before* the service signs with it — the
four-step procedure lives in `bakedInTrustedKeys`' doc comment in
[`internal/selfupdate/release.go`](../../internal/selfupdate/release.go). Full
treatment in [`../security/trust-model.md`](../security/trust-model.md) and
[`../security/key-management-and-rotation.md`](../security/key-management-and-rotation.md).
