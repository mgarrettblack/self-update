# Versions and semver comparison

**When:** Choosing the `-X ...selfupdate.Version=` value for a build, or debugging why a client will not accept a manifest it should — or accepts one it should not.

**Source of truth:** the semver section at the top of `internal/selfupdate/release.go`, and its two call sites: `Checker.Check` and `Manifest.Validate` (both in `internal/selfupdate/update.go` and `release.go` respectively). The code wins over this doc.

---

## Where this lives now

There used to be an `internal/version` package. It is **gone**. `Version`, `Semver`,
`parseSemver`, `compareSemver`, `IsNewer` and the prerelease comparator all live in
`internal/selfupdate/release.go`, in the same file as the manifest and the trust set,
because version comparison is release *data* — see [release-go.md](release-go.md) for why
those belong together.

The practical consequence: **the version ldflag path changed.**

```sh
-X self-update/internal/selfupdate.Version=1.4.2      # correct
-X self-update/internal/version.Version=1.4.2         # dead; that package no longer exists
```

## Surface

```go
var Version = "0.0.0-dev"                        // overwritten at link time
type Semver struct{ Major, Minor, Patch int; Prerelease, Build string }
func (Semver) String() string
func IsNewer(candidate, current string) (bool, error)
```

`parseSemver`, `compareSemver`, `comparePrerelease`, `compareIdentifier` and `allDigits`
are unexported: `IsNewer` is the only comparison the rest of the package needs, and
`Manifest.Validate` calls `parseSemver` directly to reject a non-semver release version at
parse time.

## The `0.0.0-dev` trap

`Version` is injected by whoever builds the client. Getting it wrong fails in two *opposite*
directions, and the more surprising one is the omission:

- **Flag omitted entirely.** `Version` stays `"0.0.0-dev"`, which **is** valid semver: core
  `0.0.0` with prerelease `dev`. `Checker.Check` therefore parses it happily and proceeds.
  But by the prerelease rule below, `0.0.0-dev` sorts **below every published release** —
  below `0.0.1`, below `0.0.0` itself. So an un-flagged build considers the very first
  manifest it sees newer than itself and **immediately replaces its own binary**. A local
  development build pointed at a real release host is not a harmless no-op; it is an instant
  self-update, complete with the swap, the marker and the relaunch. If that is not what you
  want, either pass the flag or do not point it at a real host.
- **Injected value is not semver** (a git SHA, a branch name, a bare `dev`). `Checker.Check`
  parses the running version up front, before touching the network, and returns a
  `ClassInternal` error: `running version %q is not valid semver`. Such a build can never
  update itself, and every poll logs a failure. That is the fail-closed direction — see
  [../security/fail-closed-points.md](../security/fail-closed-points.md).

Both cases are why the documented build line carries the version and the trust set
**together**; see
[dependency-rules.md](dependency-rules.md#the-two-required-build-ldflags).

## Parsing rules (stricter than `strings.Split`)

`parseSemver` splits build metadata on `+` first, then prerelease on `-`, then requires
exactly three numeric core components:

- A leading `v` is trimmed, and surrounding whitespace with it.
- Empty string, empty prerelease (`1.2.3-`) and empty build metadata (`1.2.3+`) are all
  errors — a trailing separator is a truncated version, not an equivalent one.
- Core components must be pure digits with **no leading zeros** (`01.2.3` is rejected).
  Accepting it would make `01.2.3` and `1.2.3` two spellings of one version, and nothing
  outside this code compares the manifest's `version` as a string.
- A wrong component count is named in the error (`want MAJOR.MINOR.PATCH, got 2
  component(s)`), so `1.2` is legible rather than mysterious.
- Errors always quote the **original** input, not the trimmed remainder, so a log line shows
  what the manifest actually said.

`Manifest.Validate` runs `parseSemver` on the manifest's `version` field, so a release whose
version is not semver is a `ClassManifestInvalid` failure at parse time rather than a
surprise at comparison time. See
[../release-contract/manifest-schema.md](../release-contract/manifest-schema.md).

## Comparison and prerelease precedence

`compareSemver` walks major, minor, patch, then delegates to `comparePrerelease`, which
implements semver §11.4:

1. **A version with a prerelease ranks below the same version without one.**
   `1.0.0-rc.1 < 1.0.0`. This is the rule that makes prerelease channels safe: shipping
   `1.0.0` after `1.0.0-rc.1` moves clients forward, and a client on `1.0.0` is never
   dragged back to a release candidate. It is also the rule that makes `0.0.0-dev` the
   floor of the entire version space.
2. Identifiers are compared dot-field by dot-field. All-digit identifiers compare
   numerically (`rc.2 < rc.10`, which a lexical compare gets backwards); **numeric
   identifiers always rank below alphanumeric ones** (`1.0.0-1 < 1.0.0-alpha`); otherwise
   `strings.Compare`.
3. If every shared field ties, the version with **more** identifiers wins
   (`1.0.0-alpha < 1.0.0-alpha.1`).

**Build metadata is ignored by comparison.** `+…` is parsed, preserved in `Semver.Build`,
and reproduced by `String()`, but `compareSemver` never looks at it — per semver, two
versions differing only in build metadata have equal precedence. A rebuild of the same
version with a new build tag therefore does not trigger an update, which is the point: the
release pipeline can stamp provenance into a version without every client swapping its
binary for a byte-identical one.

## `IsNewer` is strictly greater

`Checker.Check` gates on `IsNewer(m.Version, current)`, which is `compareSemver(...) > 0`.
Strictness is what handles both "already current" and a rolled-back manifest advertising an
*older* release: neither moves the client, and both are reported as a `Decision` with
`UpdateAvailable` false and a human-readable `Reason` — not as an error. A downgrade is not
expressible through this path at all. See [../update-cycle/check.md](../update-cycle/check.md).

`IsNewer` returns an error if *either* operand fails to parse. `Check` tags that error
`ClassManifestInvalid`, which is the right attribution: by the time `IsNewer` runs, the
running version has already been validated, so the only operand that can still be bad is
the manifest's.
