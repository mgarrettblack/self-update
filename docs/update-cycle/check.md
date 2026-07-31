# Checking for an update

**When:** Editing `Checker` in `internal/selfupdate/update.go`, adding a field to `Checker` or `Decision`, or debugging why a client reports "no update" / fails a check against a real release host.

**Source of truth:** `internal/selfupdate/update.go` (`Checker`, `Decision`, `Check`), `internal/selfupdate/release.go` (`ParseManifest`, `IsNewer`, `Verifier`, `InRolloutCohort`), `internal/selfupdate/net.go` (`fetchBytes`, `requireHTTPS`), `internal/selfupdate/constants.go` (`defaultMaxManifestBytes`, `maxSignatureBytes`, `signatureURLSuffix`, `defaultCheckTimeout`), `self-update-design.md` §1. The code wins if this document disagrees with it.

---

`Checker.Check` is the read-only half of the cycle. It performs no writes and
downloads nothing but the manifest and its detached signature, so a failed check
is always safe to retry and never leaves the installation in a partial state.

## Order of operations

```
1. Verifier == nil            -> hard error, no network access at all
2. ManifestURL empty          -> hard error
3. requireHTTPS(manifest URL) -> unless AllowInsecureManifestURL
4. parse the running version  -> must be valid semver
5. GET ManifestURL            -> raw bytes, capped
6. GET ManifestURL + ".sig"   -> raw bytes, capped
7. DecodeSignature            -> base64, must decode to 64 bytes
8. Verifier.Verify(body, sig) -> over the raw bytes, before any parsing
9. ParseManifest(body)        -> trusted from here on, and only from here on
10. IsNewer                   -> no  -> Decision{UpdateAvailable: false}
11. m.Artifact(platform)      -> err -> Decision{UpdateAvailable: false}
12. requireHTTPS(artifact URL)-> unless AllowInsecureArtifactURL
13. InRolloutCohort           -> no  -> Decision{UpdateAvailable: false}
14. Decision{UpdateAvailable: true, Artifact: art}
```

Steps 5–8 are [invariant 1](invariants.md): verify before parse. The comment in
the code marks the boundary — "Trusted from here on, and only from here on."

Step 4 reads `Checker.CurrentVersion`, falling back to the package-level `Version`
var when it is empty. `Version` is link-time injected and defaults to
`0.0.0-dev`, which parses as valid semver but is never newer than a published
release, so a build without its `-X` flag silently declines every update rather
than failing the check.

## Fail closed on a nil `Verifier`

```go
if c.Verifier == nil {
    return nil, classify(ClassInternal, op, errors.New(
        "no verifier configured; refusing to check for updates that cannot be verified"))
}
```

This is checked *before touching the network*, not after fetching. A client with
no trust set has nothing it could legitimately do with a manifest — the only
available action would be to act on unverified data — so the request is never
made. Combined with `TrustedVerifier()` refusing to build over an empty trust set
(see [fail-closed points](../security/fail-closed-points.md)), a build shipped
without keys fails visibly on its first check rather than degrading into "accept
anything".

## The signature URL is derived, never configured

`signatureURLSuffix = ".sig"` is appended to `ManifestURL`. There is no separate
`SignatureURL` field, deliberately: two independent URLs are a way to
misconfigure a client into verifying release A's manifest against release B's
signature.

A non-200 on the `.sig` fetch — 404 included — is a failure. It is never a
licence to treat the manifest as unsigned. The comment on the status check in
`fetchBytes` says so explicitly.

## `Cache-Control: no-cache`

Both fetches set it. Release buckets sit behind CDNs that will happily serve a
cached manifest long after a release has been pulled, which is exactly the
scenario where a stale manifest does harm: a build that was yanked for crashing
keeps reaching clients for the rest of the TTL. Signatures do not help here — the
pulled manifest was legitimately signed.

Note the related limitation recorded on `AllowInsecureManifestURL`: over
plaintext HTTP a network attacker can suppress an update indefinitely by serving a
stale manifest, and signatures do not prevent that either. HTTPS is what closes
the downgrade-by-staleness hole.

## Response caps

- `defaultMaxManifestBytes = 1 << 20` (1 MiB), overridable via `MaxManifestBytes`
- `maxSignatureBytes = 4 << 10`

`fetchBytes` reads one byte past the cap (`io.LimitReader(resp.Body, max+1)`) so
an oversized body is *detected* rather than silently truncated. Truncating would
change the bytes the signature covers and surface as a confusing signature
failure instead of the size failure it actually is. Over-cap bodies are
`ClassManifestInvalid`; transport failures are `ClassNetwork`.

The default check timeout is 30s (`defaultCheckTimeout`) when no `Client` is
supplied.

## "No update" is not an error

Three outcomes return a `Decision` with `UpdateAvailable: false`, a populated
`Reason`, and a nil error:

| Outcome | `Reason` shape |
|---|---|
| not newer (same version, or a rolled-back manifest advertising an older release) | `running X, published release is Y` |
| no artifact for this platform | `release Y publishes no artifact for os-arch` |
| outside the rollout cohort | `release Y is at N% rollout and this install is not in the cohort` |

`Poller.Update` logs the reason and carries on. Only a genuine failure —
unreachable host, bad signature, malformed document, non-HTTPS artifact URL —
returns an error.

`Decision.Manifest` is populated whenever a manifest was fetched and verified,
even when no update applies, so a caller can log what the published version is.

## HTTPS enforcement is two independent switches

`requireHTTPS` is called twice with different flags and different error classes:

- manifest URL, gated by `AllowInsecureManifestURL`, class `ClassInternal` (a
  local misconfiguration)
- artifact URL, gated by `AllowInsecureArtifactURL`, class `ClassManifestInvalid`
  (the *manifest* named a URL this client will not fetch from)

They are separate so a dev setup can serve the manifest locally while still
enforcing the real policy on what gets downloaded and executed. `cmd/app` ties
both to its single `insecure` config field.

`PlatformArtifact.validate` in `release.go` accepts `http` at the schema level
precisely so a local release host can be exercised; the `Checker` is the layer
that refuses it.

## Errors leak less than they could

`urlPath` reduces a URL to its path component for the `Op` string, keeping the
host out of messages that end up in logs adjacent to telemetry. Only the
`ErrorClass` is ever reported upstream — see
[error classes](../observability/error-classes.md).

## Related

- [rollout cohorts](rollout-cohorts.md)
- [manifest schema](../release-contract/manifest-schema.md)
- [verification order](../security/verification-order.md)
- [download and resume](download-and-resume.md) — the next step
