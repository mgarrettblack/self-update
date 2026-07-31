# Manifest Schema

**When:** Adding or changing a manifest field, or debugging a manifest the client rejects with `manifest_invalid`.

**Source of truth:** [`internal/selfupdate/release.go`](../../internal/selfupdate/release.go) (`Manifest`, `PlatformArtifact`, `ParseManifest`, `Validate`, `RolloutPercent`, `Artifact`, `PlatformKey`, the semver comparator, `InRolloutCohort`), [`internal/selfupdate/update.go`](../../internal/selfupdate/update.go) (how the fields are acted on in `Checker.Check`), design "Manifest format" section. Where this file and the code disagree, the code wins.

---

The manifest is produced by the release service, which **this repository does not
contain**. There is no in-repo tool that emits one (`cmd/releaser`, design phase
7, is unimplemented), and no test suite that fabricates one either — the
`_test.go` files and the `internal/releasetest` fixture package are gone. This
file describes a contract the client *consumes*, read out of the consuming code.

```json
{
  "version": "1.4.2",
  "rollout": 100,
  "platforms": {
    "darwin-arm64": {
      "url": "https://cdn.example.com/releases/1.4.2/app-darwin-arm64.zst",
      "sha256": "9f2c…",
      "size": 2411008
    }
  }
}
```

## Fields

| JSON | Go type | Required | Meaning |
| --- | --- | --- | --- |
| `version` | `string` | yes | The release's semver version. |
| `rollout` | `*int` | no | Cohort percentage, 0–100. **Omitted means 100.** |
| `platforms` | `map[string]PlatformArtifact` | yes, non-empty | Keyed `GOOS-GOARCH`. |
| `platforms[k].url` | `string` | yes | Absolute `https` (or `http`) URL of the compressed artifact. |
| `platforms[k].sha256` | `string` | yes | 64 lowercase hex chars over the **compressed** bytes. |
| `platforms[k].size` | `int64` | yes | Positive byte count of the **compressed** bytes. |

**Unknown fields are ignored** on decode. That is deliberate: adding a field later
(release channels, say) must not break clients already deployed in the field,
which by definition cannot be fixed except by an update they would then refuse to
take. Do not add strict-field decoding.

### `version`

Must parse as semver — `parseSemver` in
[`release.go`](../../internal/selfupdate/release.go), which tolerates a leading
`v`, rejects leading zeros in numeric components, and implements prerelease
precedence (semver §11.4: a prerelease ranks below the same version without one,
numeric identifiers rank below alphanumeric ones). `Check` installs a release only
if `IsNewer(manifest.Version, current)` — **strictly** newer. So a manifest that
advertises an older release moves nobody backwards; re-publishing 1.4.1 to undo
1.4.2 is not a rollback mechanism. (Rollback on the client is the retained `.old`
binary; see [`../update-cycle/rollback.md`](../update-cycle/rollback.md).) "Not
newer" is a `Decision` with `UpdateAvailable: false` and a `Reason`, not an error.

An unparseable `version` is `ClassManifestInvalid`. The *running* version failing
to parse is `ClassInternal` instead — that is a build that forgot its
`-X self-update/internal/selfupdate.Version=` linker flag, not a bad manifest. The
version string and the trust set now live in the same package, so both flags name
`self-update/internal/selfupdate`; see
[`../architecture/versioning.md`](../architecture/versioning.md).

### `rollout`

Typed `*int`, not `int`, and read only through `RolloutPercent()`. The pointer is
the whole point: JSON's zero value for an absent `int` is `0`, which as a rollout
percentage means **ship to nobody**. A service that omits the field would publish
a release that silently reaches no one and produces no error anywhere — an
invisible failure. `defaultRolloutPercentage` is therefore 100, and the pointer is what lets
an omitted field be distinguished from an explicit `0` (which does legitimately
mean "nobody yet").

Outside 0–100 is `ClassManifestInvalid`. Cohort membership is
`sha256(installID + "|" + version)`, first 8 bytes big-endian, `% 100 < rollout`,
decided locally by `InRolloutCohort` — deterministic in `(installID, version)` and
monotonic in the percentage, so ramping only ever adds clients, and `0` and `100`
short-circuit. Being outside the cohort is "no update", not an error. See
[`../update-cycle/rollout-cohorts.md`](../update-cycle/rollout-cohorts.md).

### `platforms`

Keys are `runtime.GOOS + "-" + runtime.GOARCH` (`PlatformKey()`), e.g.
`darwin-arm64`, `linux-amd64`, `windows-amd64`. `Checker.Platform` can override
the key the client looks up.

A release that omits *this* client's platform is a **`Decision` with
`UpdateAvailable: false`**, not an error — `Check` catches the error from
`Manifest.Artifact` and converts it into a `Reason`. A staggered platform rollout
must not fill logs with failures on the platforms that are not out yet. An
entirely **empty** `platforms` map, by contrast, is `ClassManifestInvalid`: that
is a broken manifest, not a partial release.

### `url`, `sha256`, `size`

`sha256` and `size` describe the **compressed** artifact — the exact bytes that
cross the wire — not the binary inside it. This is what makes verify-before-
decompress possible; see [artifact-format.md](artifact-format.md).

`PlatformArtifact.validate()` enforces:

- `sha256` exactly 64 chars, all in `0123456789abcdef`. `ParseManifest`
  lower-cases and trims the digest first, so an upper-case digest is accepted —
  hex case is not meaning. `Downloader.Fetch` normalises again and re-runs
  `validate()`, so a hand-built artifact cannot bypass the manifest's own rules.
- `size` strictly positive, so a zero or negative size is `ClassManifestInvalid`
  at parse time. The space preflight has its own backstop for a non-positive
  requirement reaching it by another route, and classifies that `ClassInternal`
  rather than `ClassDiskFull`, so a bad `size` cannot be mistaken in telemetry for
  a fleet of full disks.
- `url` parses, is absolute, and has a non-empty host. Scheme must be `https` or
  `http`; `http` is accepted *here* only so a release host can be exercised
  locally, and `Checker` still rejects it unless
  `AllowInsecureArtifactURL` is set.

## Parse order and the double-check

`ParseManifest` is called **only** on bytes whose signature already verified —
parsing earlier means acting on attacker-controlled URLs. See
[signature-format.md](signature-format.md) and
[`../security/verification-order.md`](../security/verification-order.md).

The client validates a manifest it has already cryptographically trusted, which
is not redundant: a signature proves the manifest came from the release pipeline,
not that the pipeline got it right. A signed manifest with a 63-character digest
is still a broken release.
