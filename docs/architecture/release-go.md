# release.go — the release-data layer

**When:** Changing anything about what a release *is* — semver comparison, the manifest schema, signature verification, the trust set, or rollout cohorts — or navigating the ~490-line file to find one of those five things.

**Source of truth:** `internal/selfupdate/release.go`, plus `internal/selfupdate/doc.go` for where it sits in the layering. The code wins over this doc.

---

## What it is

`release.go` holds everything the client needs in order to decide **what a release is and
whether to believe it**. It is layer 2 of the package (see
[dependency-rules.md](dependency-rules.md#the-layering-rule-now-intra-package)): it depends
on `errors.go` for classification and on nothing else of ours. It performs no I/O — no
network, no filesystem — which is why it can be read and reasoned about without tracing
through the update cycle.

It is a merge of four former files (`keys.go`, `manifest.go`, `rollout.go`, `verify.go`)
plus the semver code from the deleted `internal/version` package.

## Why these five things are one file

They look like five topics. They are one: the decisions that turn a stream of untrusted
bytes into a release this client is willing to act on. Every one of them is answered before
a single byte of an artifact is requested, and each depends on the one before it.

- **Semver** and **manifest parsing** cannot be separated: `Manifest.Validate` calls
  `parseSemver` on the manifest's `version`, so a release whose version is not semver is a
  parse-time failure rather than a surprise at comparison time. Splitting them put the
  comparator in its own package, and the only thing that bought was an import edge.
- **Manifest parsing** and **signature verification** are ordered against each other, and
  that order is a security property: the signature is checked over the raw bytes as
  received, *before* `ParseManifest` runs, so nothing untrusted reaches a parser. Two files
  can state that invariant; one file lets you see both halves of it at once.
  `ParseManifest`'s doc comment says so explicitly, right above the function.
- **The trust set** is only meaningful as an input to the `Verifier` immediately above it,
  and the rotation procedure it documents is about which signatures verify.
- **Rollout cohorting** reads `Manifest.RolloutPercent()` and keys on the manifest's
  version. It is the last gate applied to a manifest that has already been verified, parsed
  and found newer — the natural end of the same sequence.

What is deliberately *not* here: fetching (that is `net.go`), anything touching disk (that
is `fs.go`), and the sequencing of the gates (that is `Checker.Check` in `update.go`).
`release.go` supplies the predicates; `update.go` decides the order they run in.

## Section map

The file reads top to bottom in five blocks. Approximate landmarks:

| Section | Around | Contents |
| --- | --- | --- |
| **Semver** | L18–166 | `Version` (link-time var, default `"0.0.0-dev"`), `Semver` + `String`, `parseSemver`, `parseNumericIdentifier`, `compareSemver`, `IsNewer`, `comparePrerelease`, `compareIdentifier`, `allDigits`. Detail in [versioning.md](versioning.md). |
| **Manifest** | L168–287 | `defaultRolloutPercentage` (100, because JSON's zero for an absent int would mean "ship to nobody"), `Manifest`, `PlatformArtifact`, `RolloutPercent`, `Artifact`, `PlatformKey`, `ParseManifest`, `Manifest.Validate`, `PlatformArtifact.validate`. Detail in [../release-contract/manifest-schema.md](../release-contract/manifest-schema.md). |
| **Verification** | L289–382 | `Verifier`, `NewVerifier`, `Verify`, `ParsePublicKey`, `ParsePublicKeys`, `DecodeSignature`. Detail in [../security/verification-order.md](../security/verification-order.md) and [../release-contract/signature-format.md](../release-contract/signature-format.md). |
| **The trust set** | L384–455 | `TrustedKeysBase64`, `bakedInTrustedKeys` (with the four-step rotation procedure in its doc comment), `TrustedVerifier`. Detail in [../security/trust-model.md](../security/trust-model.md) and [../security/key-management-and-rotation.md](../security/key-management-and-rotation.md). |
| **Rollout** | L457–487 | `InRolloutCohort`. Detail in [../update-cycle/rollout-cohorts.md](../update-cycle/rollout-cohorts.md). |

Navigate by type or function name, not line number — the blocks are stable, the offsets are
not.

## Things in here that are easy to get wrong

**`Manifest.Rollout` is a `*int`.** That is what makes an omitted field distinguishable
from an explicit `0`. Read it through `RolloutPercent()`, never directly; a nil pointer
means 100.

**Unknown JSON fields are tolerated on decode.** Deliberate: adding a field later (release
channels, say) must not break clients that predate it.

**`Validate` re-checks what the release service already checked.** A signature proves the
manifest came from the release pipeline, not that the pipeline got it right. `validate`
insists on a 64-character lowercase hex digest, a positive size, and an absolute URL;
`ParseManifest` lower-cases and trims digests first so a case difference is not a failure.

**`validate` accepts `http`, and that is not the policy.** It permits `http` only so a
release host can be exercised locally; the real transport decision is `requireHTTPS` in
`net.go`, called from `Checker.Check` with the manifest and artifact switches kept
independent. Do not "fix" `validate` to reject `http` — you would remove the local
development path without adding any protection the checker does not already give.

**A `Verifier` is a set, never one key.** Rotating away from a compromised key requires
that already-deployed clients accept a signature from the replacement; a client that trusts
exactly one key can never be migrated off it. `Verify` tries every key and returns
`ClassSignatureInvalid` if none match — there is no partial success and no fallback that
accepts unverified bytes.

## The trust set

`TrustedVerifier()` builds the verifier from **two sources, unioned**:

1. `bakedInTrustedKeys` — a `[]string` committed to source, listed first.
2. `TrustedKeysBase64` — the link-time flag, split on commas, and only split at all when it
   holds something, so an unset flag contributes no entries rather than one empty string.

Entries are trimmed, blanks are skipped (a trailing comma in a build flag is harmless), and
duplicates are dropped, so a key present in both sources counts once. Order does not affect
acceptance: `Verify` tries every key.

**In this repository `bakedInTrustedKeys` is empty** — it is declared as a `[]string` with
only a comment inside, saying no keys are committed here and to supply them via `-ldflags`
or add the project's release public keys to the slice. So today the union is exactly what
the flag provides, and **`TrustedKeysBase64` is effectively mandatory**: without it,
`TrustedVerifier()` returns a `ClassInternal` error whose message names the exact flag to
pass.

Having both sources is what makes rotation survivable. The procedure is in
`bakedInTrustedKeys`'s doc comment and the ordering is the whole point: **the fleet must
trust a new key before the service signs with it.** Add the new key to the slice while
keeping the outgoing one, ship that build, wait until effectively every client runs it,
*then* switch the service to the new key, and only remove the old key once no supported
client trusts it alone. Skipping the wait strands every client that has not yet updated —
it rejects all future releases as unsigned and can never update itself out of that state
without a manual reinstall.

**`TrustedVerifier()` fails closed on an empty set** rather than returning a permissive
verifier. A build with no keys would otherwise have to choose at runtime between "reject
everything" and "accept anything", and the second is a remote code execution vector.
Refusing at construction makes the mistake visible at startup — `cmd/app` treats it as
fatal — instead of on the first check. Never add a code path that reads a trust anchor from
config, an environment variable or the network: a public key an attacker can replace
reduces signature verification to theatre. See
[../security/fail-closed-points.md](../security/fail-closed-points.md).

## Rollout cohorting

`InRolloutCohort(installID, releaseVersion, percent)` hashes `installID + "|" + version`
with SHA-256 and takes the first eight bytes modulo 100. Two properties matter, and both
follow from that construction:

- **Deterministic in (installID, version).** A client that re-rolled on every poll would
  drift into any cohort eventually, so a 10% rollout would reach everyone given enough
  hours.
- **Monotonic in percent.** Raising the percentage only ever adds clients, so ramping a
  release never makes it disappear from a client that already saw it.

Keying on the version as well as the install ID is what stops the same unlucky 10% of the
fleet being the canary for every release forever. `percent <= 0` is nobody and
`percent >= 100` is everybody, short-circuited before hashing.

Staged rollout is the fleet-level half of the safety story; crash-loop detection
(`Guard`, in `update.go`) is the per-machine half. Neither substitutes for the other: a
marker catches a bad update on one machine after the fact, and does nothing to stop a bad
release reaching everyone at once.
