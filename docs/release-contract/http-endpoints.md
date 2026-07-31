# HTTP Endpoints the Client Requests

**When:** Standing up or debugging a release host, or changing anything in `Checker.Check` / `Downloader` that touches URLs, request headers, HTTP status handling or resume.

**Source of truth:** [`internal/selfupdate/update.go`](../../internal/selfupdate/update.go) (`Checker.Check`, the body caps, `signatureURLSuffix`), [`internal/selfupdate/net.go`](../../internal/selfupdate/net.go) (`fetchBytes` for the manifest and signature, `Downloader.Fetch`/`attempt` for the artifact, `requireHTTPS`), [`README.md`](../../README.md), design §1, §4. Where this file and the code disagree, the code wins.

---

The release service is **not in this repository**. It is a separate service that
this repo does not own: no key generation, no build, no compression, no signing,
no hosting code exists here. The design's `cmd/releaser` and `cmd/devserver`
(phase 7) are unimplemented, so there is no in-repo way to produce a real signed
release — do not go looking for server code. There is also **no test suite**: the
`_test.go` files and the `internal/releasetest` fixture package have been removed,
so nothing in this repository exercises these requests end to end. Everything
below is read out of the client's code, and a change to a release host has to be
validated against a real client build.

The client is configured with **one** URL and derives everything else.

| # | Request | Response body | Consumed by |
| --- | --- | --- | --- |
| 1 | `GET <manifest-url>` | the manifest JSON — see [manifest-schema.md](manifest-schema.md) | `Checker.Check` via `fetchBytes` |
| 2 | `GET <manifest-url>.sig` | one base64 Ed25519 signature — see [signature-format.md](signature-format.md) | `Checker.Check` via `fetchBytes` |
| 3 | `GET <artifact url from the manifest>` | the zstd-compressed binary — see [artifact-format.md](artifact-format.md) | `Downloader.Fetch` |

Static file hosting is sufficient — the design's Assumptions table commits to
"static file hosting only (S3/CloudFront, GCS, or GitHub Releases)" and to no auth
on the update endpoint, since integrity and authenticity come from the signature
rather than from access control. There is no POST and no per-client API call: the
rollout cohort is decided locally, so the service tracks nothing.

## The signature URL is derived, never configured

`Checker` has a single `ManifestURL` field. The signature URL is
`ManifestURL + signatureURLSuffix`, where `signatureURLSuffix` is the constant
`".sig"`.

This is a deliberate absence of configuration. A client with a separate
`SignatureURL` field could be misconfigured — by a typo, a stale template, a
half-finished rollout of a new bucket path — into checking release A's manifest
against release B's signature. Both would be authentic, both would verify
against a trusted key, and the mismatch would be invisible. Deriving the URL
removes the failure mode instead of documenting it. Do not add a field for it.

## Request 1 and 2: manifest and signature

Both go through the same `fetchBytes` function in `net.go` — a package-level
function, not a `Checker` method — so the two requests cannot drift apart. Both
carry the same headers:

- **`User-Agent`** — only if `Checker.UserAgent` is set. `cmd/app` sets
  `"<appName>/<version>"`.
- **`Cache-Control: no-cache`** — on **both** requests. Release buckets sit behind
  CDNs that will happily serve a cached manifest long after a release has been
  pulled. Pulling a bad release is the emergency lever; a cached copy defeats it.

Response handling:

- Anything other than `200` is a `ClassNetwork` failure, **including `404`**. A
  missing `.sig` is never a licence to treat the manifest as unsigned. Note the
  class: a 404 on the signature reports as `network`, not `signature_invalid`.
- Bodies are read through `io.LimitReader(body, max+1)` and rejected as
  `ClassManifestInvalid` if they exceed `max` — one byte past the cap so an
  oversized body is *detected* rather than silently truncated. Truncation would
  change the bytes the signature covers and surface as a confusing signature
  failure instead of a size failure.
- Caps: `Check` passes `defaultMaxManifestBytes` = 1 MiB (override with
  `Checker.MaxManifestBytes`) for the manifest and `maxSignatureBytes` = 4 KiB
  (fixed) for the signature. Both constants are declared in `update.go` and are
  arguments to `fetchBytes`, which has no cap of its own.
- `defaultCheckTimeout` = 30s when `Checker.Client` is nil.

`fetchBytes` does **not** set `Accept-Encoding`, so Go's transport may negotiate
gzip and transparently decode. What the client verifies is the body it receives
after that decoding, byte for byte — so the service must serve the same document
bytes regardless of encoding. (Contrast request 3, where the digest is over the
stored bytes and the header is therefore pinned.) See
[signature-format.md](signature-format.md) for why byte fidelity is absolute
here.

Failing closed happens *before* the network: a nil `Verifier`, an empty
`ManifestURL`, or a non-HTTPS URL without an explicit waiver all error out
without a request being made. There is nothing useful a client can do with a
manifest it cannot verify. See
[`../security/fail-closed-points.md`](../security/fail-closed-points.md).

## Request 3: the artifact

`Downloader.attempt` sets:

- **`User-Agent`** — only if `Downloader.UserAgent` is set.
- **`Accept-Encoding: identity`** — always. Left alone, the transport advertises
  gzip and transparently decompresses, so the bytes hashed would not be the bytes
  the release pipeline signed, and `Content-Length` would describe neither.
  Setting the header also disables Go's automatic decoding, so a server that
  compresses anyway fails the digest check rather than slipping through.
- **`Range: bytes=<offset>-`** — only when a partial file at the destination is a
  usable prefix (`resumeOffset`: regular file, non-empty, strictly smaller than
  the manifest's `size`).

A service that ignores `Range` costs correctness nothing — it just gets a restart
from zero:

| Status | Client behaviour |
| --- | --- |
| `200` | Server ignored `Range`; offset reset to 0 and the file truncated. Appending would concatenate a prefix with a full copy and silently corrupt it. |
| `206` | Append only if `Content-Range`'s first byte position parses and equals the requested offset. Missing or disagreeing → full restart, never a guess. |
| `416` | Delete the partial. Retryable only if a range *was* requested; an unranged 416 would answer identically next time. |
| `408`, `429`, `5xx` | Retryable. |
| other | Not retryable. |

Two content-level rejections are deliberately non-retryable, because repeating
the request cannot change the answer: a `Content-Length` that contradicts the
manifest's remaining byte count (`ClassHashMismatch`, partial deleted), and a
final digest mismatch (`ClassHashMismatch`, file deleted — a partial that
disagrees with the manifest is not a prefix worth resuming, and leaving it would
poison every later attempt forever).

Retry budget: `defaultFetchAttempts` = 4, `defaultBaseBackoff` = 1s doubling to a
`maxBackoffDelay` of 30s, with equal jitter so a CDN blip does not produce a
synchronised retry herd. `defaultFetchTimeout` = 15 min bounds one attempt.
Update checks repeat hourly, so grinding here has no value.

## Transport security

Non-HTTPS is refused by `requireHTTPS` unless explicitly waived. The two
`Checker` fields `AllowInsecureManifestURL` and `AllowInsecureArtifactURL` still
exist and are **independent**, so a dev setup can serve the manifest from a local
HTTP server while still enforcing real policy on what gets downloaded and
executed. `cmd/app`'s single `insecure` config field sets both — a demo-app convenience,
not the library's model.

Signatures do not make plaintext safe: an attacker who can rewrite responses can
suppress updates indefinitely by replaying a stale but authentically signed
manifest. Insecure URLs are for local testing only.

Related: [`../update-cycle/check.md`](../update-cycle/check.md) and
[`../update-cycle/download-and-resume.md`](../update-cycle/download-and-resume.md)
for where these requests sit in the cycle,
[`../observability/error-classes.md`](../observability/error-classes.md) for how
the error classes are reported.
