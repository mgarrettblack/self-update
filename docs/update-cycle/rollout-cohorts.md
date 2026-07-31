# Staged rollout cohorts

**When:** Editing `InRolloutCohort` in `internal/selfupdate/release.go`, changing the `rollout` field's semantics, or answering "why did this client not get the release / why did it get it twice".

**Source of truth:** `internal/selfupdate/release.go` (`InRolloutCohort`, `Manifest.RolloutPercent`, `Manifest.Validate`), `internal/selfupdate/constants.go` (`defaultRolloutPercentage`), `internal/selfupdate/fs.go` (`InstallID`), `internal/selfupdate/update.go` (`Checker.InstallID`), `self-update-design.md` §3. The code wins if this document disagrees with it.

---

## The rule

```go
sum := sha256.Sum256([]byte(installID + "|" + releaseVersion))
bucket := binary.BigEndian.Uint64(sum[:8]) % 100
return bucket < uint64(percent)
```

with two short-circuits: `percent <= 0` is nobody, `percent >= 100` is everybody.
That is the whole of `InRolloutCohort`, and it is the entire cohorting mechanism.

## Why this exists at all

Crash-loop detection (see [rollback](rollback.md)) catches a bad update on one
machine after the fact. It does nothing to stop a bad release reaching the whole
fleet at once. Staged rollout is the other half of §3: publish at 10%, watch the
telemetry, then ramp.

## Determinism in `(installID, version)`

The hash input is the install ID *and* the release version. Both halves matter,
for different reasons.

**Deterministic per client and release.** A client re-evaluates its cohort on
every poll — hourly. If the decision were random, or keyed on anything that
changes between polls, a 10% rollout would reach everyone given enough hours:
each client would keep drawing until it drew a winning bucket. `InstallID` is
persisted to `<state-dir>/install-id` for exactly this reason, and its doc comment
names the failure: "an id that changed every boot would let a 10% release reach
the whole fleet by attrition."

**Reshuffled per release.** Keyed on the install ID alone, the same unlucky 10% of
the fleet would be the canary for every release forever — those machines would
absorb every bad build while the other 90% never exercised a rollout at all.
Mixing the version in redraws the cohort for each release, so canary duty rotates.

The `|` separator is not decorative: it keeps `("ab", "1.0")` and `("a", "b1.0")`
from hashing identically.

## Monotonic in percent

The bucket is a property of `(installID, version)` only; `percent` appears solely
in the final comparison. So raising the percentage can only ever *add* clients.
A release ramped 10% → 25% → 100% never disappears from a client that already saw
it at 10%, which would otherwise present as a client that offered an update,
failed, and then reported "no update" on the retry — an unexplainable state from
the field.

Both properties are structural rather than incidental: they follow from the hash
input being fixed and `percent` appearing nowhere but the comparison. Any change
that mixes `percent` into the hash, or drops the version from it, breaks one of
them silently — the function keeps returning plausible booleans.

## No server-side tracking

The decision is computed entirely on the client from two values it already has:
its own install ID and the version string in the signed manifest. The release side
stays static file hosting — no assignment service, no enrollment table, no
per-client state, nothing to keep consistent across a CDN. Ramping a release is
editing one integer in `manifest.json` and re-signing it.

This is what keeps the "no custom backend logic for distributing binaries"
assumption intact while still supporting canaries.

## Why an omitted `rollout` means 100, not 0

`Manifest.Rollout` is `*int`, and `defaultRolloutPercentage = 100`:

```go
// defaultRolloutPercentage is what an omitted "rollout" field means. It must be 100:
// JSON's zero value for an absent int would otherwise mean "ship to nobody",
// which is a silent, invisible failure mode.
```

Two decisions are packed in here.

The pointer makes an absent field distinguishable from an explicit `0`. With a
plain `int`, `"rollout": 0` and no `rollout` key at all would both decode to zero
and be indistinguishable.

The default of 100 is chosen because the alternative fails silently. A release
published without a `rollout` field is the ordinary case — full rollout — and if
that decoded to 0 the release would reach nobody, with every client cheerfully
logging "release 1.4.2 is at 0% rollout and this install is not in the cohort" and
no error anywhere. Nothing alerts; the release just does not happen. Defaulting
high means the failure mode of a forgotten field is "shipped to everyone", which
is at least visible.

Always read the field through `Manifest.RolloutPercent()`, never `*m.Rollout`
directly.

## Validation and clamping

`Manifest.Validate` rejects a `rollout` outside 0–100 as `ClassManifestInvalid`,
so an out-of-range value never reaches `InRolloutCohort` through the normal path.
`InRolloutCohort` still clamps defensively at both ends — it is exported and
reachable from a hand-built manifest.

## Empty install ID

An empty `installID` still produces a deterministic decision (the hash of
`"|1.4.2"`). `Checker.InstallID` documents this. It is not an error, but every
client with an empty ID lands in the same bucket for a given release, so they move
as one block — worth knowing when debugging a fleet that all updated or all
did not.

## Related

- [check](check.md) — where `InRolloutCohort` is consulted
- [manifest schema](../release-contract/manifest-schema.md)
- [state and markers](state-and-markers.md) — where `install-id` lives
- [rollback](rollback.md) — the other half of §3
