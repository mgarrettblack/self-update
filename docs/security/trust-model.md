# Trust Model

**When:** Before editing the trust set or verifier in `internal/selfupdate/release.go`, before adding any new way for the client to learn a public key, or when answering "where do the trusted keys come from and what happens if there are none?"

**Source of truth:** `internal/selfupdate/release.go` (`TrustedKeysBase64`, `bakedInTrustedKeys`, `TrustedVerifier`, `ParsePublicKey`, `ParsePublicKeys`, `Verifier`, `NewVerifier`, `Verify`), `internal/selfupdate/update.go` (`Checker.Check`'s nil-verifier guard), `self-update-design.md` §1, `README.md` ("The trust model"). If this doc and the code disagree, the code wins.

---

## The whole trust anchor is compiled in

The client carries a **trust set**: zero or more Ed25519 public keys fixed at build
time. Every release manifest must carry a detached signature from one of those keys.
That set is the only thing in the system the client trusts a priori, and it comes from
exactly two places, both compile-time, both in `release.go`:

- `bakedInTrustedKeys` — a `[]string` of standard-base64 keys committed to source.
  **It is empty in this repository**; its body is a comment saying so and pointing at
  the two ways to populate a trust set. Nothing ships a key here.
- `TrustedKeysBase64` — a package-level `var` injected by the linker:

  ```sh
  go build -ldflags "-X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
  ```

`$PUBKEY` is a **comma-separated list of standard-base64** (not URL-safe, not raw)
Ed25519 public keys.

## What `TrustedVerifier()` actually does

Precisely, in order:

1. If `TrustedKeysBase64` is blank after trimming, it contributes **no** entries. It is
   only `strings.Split` on `","` when it holds something — otherwise an unset build flag
   would contribute one spurious empty string and mask the empty-set check below.
2. The two sources are concatenated, `bakedInTrustedKeys` first, then the configured
   entries. The concatenation copies rather than appending into the package slice, so
   repeated calls cannot grow the baked-in set.
3. Each entry is trimmed; blanks and exact duplicates are dropped. So a trailing comma in
   a build script is harmless, and a key listed both in source and in `-ldflags` is
   counted once.
4. If **nothing** survives, it returns a `ClassInternal` error naming the `-ldflags`
   invocation. It does not return a permissive verifier, and it does not return
   `nil, nil`.
5. Otherwise the surviving entries go through `ParsePublicKeys` → `NewVerifier`.

`ParsePublicKey` rejects anything that does not decode as standard base64 to exactly
`ed25519.PublicKeySize` bytes, so a truncated or mangled flag fails at construction
rather than silently trusting a garbage key.

The practical consequence: **because `bakedInTrustedKeys` is empty as committed, a build
of this repository without `-ldflags` has an empty trust set and its updater refuses to
start.** That is the intended behaviour, not a bug to route around. The two-source
merge exists so a fork *can* commit its release keys to source and review them in git
history; it does not weaken the empty-set check, because the check runs on the merged
result.

## Why keys are never read at runtime

There is no code path that reads a trust anchor from a config file, an environment
variable, a state directory, or the network — and adding one would be the single most
damaging change possible to this codebase. `TrustedKeysBase64`'s doc comment states the
reason directly: a public key read at runtime is a public key an attacker can replace,
which reduces signature verification to theatre. An updater that downloads its own trust
anchor has no trust anchor: whoever controls the download controls what the client will
accept, and the client's whole job is to fetch and execute code.

This is the asymmetry that makes the design work. HTTPS protects the *transport*; the
trust set protects against everyone who can serve bytes over that transport — a
compromised CDN, a poisoned bucket, an operator with write access to release storage.
See [threat-model.md](threat-model.md) for what that does and does not cover.

## It is a set, not a key

`Verifier` holds `[]ed25519.PublicKey` and `Verify` accepts a signature that matches
*any* member. That plurality is deliberate and load-bearing for rotation, per the
doc comment on `Verifier`: a client that trusts exactly one key can never be migrated
off it, because accepting the replacement key would require an update signed by the
replacement key. Ship the set with a single member if you like, but keep it a set.
The rotation procedure is in
[key-management-and-rotation.md](key-management-and-rotation.md).

`Verify` has no partial success and no fallback. A wrong-length signature and a
signature no key matches both return `ClassSignatureInvalid`; nothing downstream is
allowed to interpret a failure as "probably fine".

## A build with an empty trust set refuses to run its updater

`TrustedVerifier()` returns an error:

```
no trusted public keys are compiled in; build with
-ldflags "-X self-update/internal/selfupdate.TrustedKeysBase64=<base64 key>"
```

`NewVerifier` likewise rejects an empty `keys...`. And `Checker.Check` refuses to make
a request at all when `Verifier` is nil, *before* touching the network.

All three matter:

- **Erroring instead of being permissive.** A build with no keys would otherwise have
  to pick at runtime between "reject every release" and "accept anything". The second
  is remote code execution for anyone who can answer an HTTP request. The first is
  quieter but still wrong: an updater that can never accept anything silently stops
  updating forever, which is why `NewVerifier` rejects an empty set rather than
  returning a verifier that always fails. Failing at construction makes the
  misconfiguration visible on the first check, in the process's own logs, instead of
  months later during an incident.
- **Checking the merged set, not either source.** The empty-set test in
  `TrustedVerifier` runs after `bakedInTrustedKeys` and `TrustedKeysBase64` are combined
  and de-duplicated. Neither source is individually required, and neither can be relied
  on to be non-empty — which is why the check cannot be moved into `ParsePublicKeys` or
  inferred from the build flag alone.
- **Failing before the network.** There is nothing useful a client can do with a
  manifest it cannot verify, so fetching one is pure downside: it burns bandwidth,
  reveals the installation to the release host, and — worst — creates the temptation
  for a future patch to "just parse it for the version number". `Check`'s nil-verifier
  guard is the first thing in the function for that reason.

Because a missing trust set is a hard failure, `cmd/app` calls `TrustedVerifier()`
first in `newPoller`, before resolving directories or the install ID: everything else
is pointless without it, and `run` treats the error as fatal (exit 1) rather than as a
warning, unlike a failed check.

## Consequences for anyone changing this

- Do not add a `Verifier` construction path that can produce an empty or
  accept-everything verifier, and do not make `Checker.Verifier` optional.
- Do not introduce a "trusted keys file", a `--pubkey` flag, or a key-discovery
  endpoint. The correct way to change what a fleet trusts is a new build.
- Keys are public, so committing them to `bakedInTrustedKeys` leaks nothing. The
  private half lives offline in the release pipeline (§1) and is not this repo's
  concern — see [threat-model.md](threat-model.md) on the release service being
  outside the trust boundary.
- Nothing in this repository can produce a signed release, and there is no test suite
  that exercises the trust path end to end. A change here is verified by reading it.

Related: [verification-order.md](verification-order.md) for what the trust set is
actually used on, [fail-closed-points.md](fail-closed-points.md) for the full list of
choose-failure-over-permissiveness points,
[../architecture/release-go.md](../architecture/release-go.md) for how `release.go` is
laid out, and
[../release-contract/manifest-schema.md](../release-contract/manifest-schema.md) for the
document being signed.
