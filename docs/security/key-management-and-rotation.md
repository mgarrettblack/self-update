# Key Management and Rotation

**When:** Before adding, removing or replacing a key in `bakedInTrustedKeys` / `TrustedKeysBase64`, when a signing key is suspected compromised, or when asked "how do we rotate the release signing key without bricking the fleet?"

**Source of truth:** the doc comment on `bakedInTrustedKeys` in `internal/selfupdate/release.go`, plus `Verifier`, `TrustedVerifier` and `ParsePublicKey` in the same file, and `README.md` ("Key rotation, from the client's side"). If this doc and the code comment disagree, the code wins.

---

## The client does not own the keys

The **release service owns the keypair.** The private half lives offline in that
service's CI/release pipeline (§1); this repository contains no key generation, no
signing, and no `cmd/releaser` — the design's phase 7 is not implemented here. Nothing
in this repo can produce a signed release, and there is no test suite that fabricates
one either.

The client's entire responsibility is the *public* half: which keys it will accept a
manifest signature from. That is the compile-time trust set described in
[trust-model.md](trust-model.md). So "rotating a key" is a two-party operation, and the
client side of it is always **ship a build that trusts the new key** — nothing more, and
nothing that can be done at runtime.

## The four-step procedure

From the doc comment on `bakedInTrustedKeys`, verbatim in substance:

1. **Get the new public key** from whoever runs the release service, add it to
   `bakedInTrustedKeys` (or to the `-ldflags` list) **keeping the outgoing key in
   place**, and ship that build.
2. **Wait until effectively every client is running a build that trusts both keys.**
   Until that point, releases must still be signed by the old key.
3. **Only then** does the release service switch to signing with the new key.
4. **Once no supported client trusts the old key alone, remove it** from the trust set.

Steps 1 and 3 are ordinary work. Step 2 is the whole procedure.

## Step 2 is the one that can brick the fleet

> Skipping step 2 strands every client that has not yet updated: it will reject all
> future releases as unsigned and can never update itself out of that state without
> manual reinstallation.

The failure is circular, and worth spelling out because it is not recoverable in the
field:

- A client trusting only key A is running in the field.
- The service starts signing with key B before that client has picked up a build
  trusting B.
- The client fetches the new manifest, `Verifier.Verify` finds no trusted key that
  matches, and it fails with `ClassSignatureInvalid` — correctly. This is the fail-closed
  behaviour working as designed; there is no "accept anyway" branch and there must never
  be one.
- The only build that would teach the client to trust B is *itself signed with B*. The
  updater cannot deliver its own fix.
- Every subsequent release fails the same way, forever. The client is now permanently
  frozen at its current version and needs a manual reinstall — on every affected
  machine, by a human, possibly a customer.

Worse, this looks exactly like an attack in telemetry. `ClassSignatureInvalid` is
`IsTamperSignal()` and escalates to `SeverityAlert`, so a botched rotation floods the
alerting channel that exists to detect a compromised CDN. Expect that, and do not tune
the alert down in response — fix the rotation. See
[../observability/error-classes.md](../observability/error-classes.md).

The overlap window in step 2 is therefore governed by how long the *slowest* supported
client takes to update: laptops that are offline for weeks, machines behind firewalls,
installations that were paused. Combine that with staged rollout (a release at 10%
reaches 10% of the fleet, not all of it) and the honest answer is that the window is
measured in weeks or longer, and should be driven by telemetry showing the version
census, not by a calendar guess. `ReportNoUpdate` on the `Poller` exists precisely
because that census is sometimes worth the traffic.

## Why the trust set is plural from day one

`Verifier` is a set even when only one key is populated, and its doc comment gives the
reason: *"a client that trusts exactly one key can never be migrated off it."* The
plurality is not a convenience for multi-key setups — it is what makes step 1 possible
at all. Do not "simplify" `Verifier` to a single key, and do not add a fast path that
bypasses the loop in `Verify`.

`Verify` accepts a signature matching **any** member of the set, with no notion of key
preference, key ID, or expiry. During an overlap window a client will happily accept
either key. That is intended: expressing an ordering would let the client refuse a
signature the service legitimately produced, which reintroduces the stranding failure
from a different direction. (`TrustedVerifier` does put `bakedInTrustedKeys` ahead of the
`-ldflags` entries when it builds the list, but that is list construction, not
precedence — `Verify` tries every member until one matches.)

## Compromise: rotation is not revocation

If a private key is compromised, adding a new key does **not** stop the old one from
working. Until every client has shipped a build in which the old key is *removed*
(step 4), a client that still trusts it will accept a manifest signed with it — and an
attacker holding that key plus the ability to serve responses (see
[threat-model.md](threat-model.md)) can ship arbitrary code to those clients. Steps 1–3
buy the ability to move; step 4 is the one that actually revokes, and it cannot be
completed faster than the fleet updates.

There is no client-side revocation mechanism, deliberately: a revocation list would have
to be fetched, which makes it an attacker-suppressible input to a trust decision — the
same objection that keeps keys out of config and off the network. If a compromise
requires faster containment than the fleet can update, that containment has to happen at
the release service and its hosting, not here.

## Mechanics when you make the change

- Both sources are merged, trimmed and de-duplicated by `TrustedVerifier()`, so a key
  may live in `bakedInTrustedKeys`, in `-ldflags`, or both; listing it twice is
  harmless. The merge is a copy — `append(append([]string{}, bakedInTrustedKeys...),
  configured...)` — so nothing mutates the package-level slice.
- `bakedInTrustedKeys` is **empty as committed** in this repository. A build that does
  not pass `-ldflags` therefore has no trust set at all and its updater refuses to
  start; see [trust-model.md](trust-model.md).
- Keys are **standard base64** of the raw 32-byte Ed25519 public key. `ParsePublicKey`
  rejects anything else at construction, so a wrong encoding fails the build's first
  update check rather than a later signature comparison.
- The list separator is a comma, and blank entries are skipped — a trailing comma in a
  release script is fine. An entirely blank `TrustedKeysBase64` contributes nothing
  rather than one empty entry, which is what keeps the empty-set check meaningful.
- The linker flag path is `-X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY`.
  The trust set and the version string now live in the same package, so a build needs
  `-X self-update/internal/selfupdate.Version=<semver>` alongside it; see
  [../architecture/versioning.md](../architecture/versioning.md).
- Removing the last key leaves an empty trust set, and the updater then refuses to run
  at all rather than accepting unsigned releases. That is the intended behaviour; see
  [fail-closed-points.md](fail-closed-points.md).
- Public keys are not secrets. Committing them to `bakedInTrustedKeys` is fine and makes
  the trust set reviewable in git history, which is the main argument for putting them
  there rather than only in build flags.
- Nothing here is covered by tests. Verify a rotation change by reading
  `TrustedVerifier` and by checking that a build with the new flag starts and completes
  one update check against a real release host.
