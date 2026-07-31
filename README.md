# self-update

The client half of a self-updating Go application: it asks a release service
what the current version is, verifies the answer is signed by a key it already
trusts, replaces its own binary atomically, and rolls back if the new build
cannot start.

Everything here runs on the customer's device. Producing releases — generating
signing keys, compiling artifacts, signing manifests, hosting them — belongs to
a separate release service that this repository does not own and does not
contain. The client is a consumer of that service's HTTP API.

Implements the client side of [self-update-design.md](self-update-design.md).

```
internal/selfupdate/    the updater library
cmd/app/                a demo application that keeps itself up to date
```

The library is one package, and its files are grouped by distance from the OS
rather than by topic — so a reader tracing one update cycle moves down the stack
instead of hopping between eight files:

```
doc.go          the layer map and the ordering invariants
constants.go    every tunable, grouped by layer
errors.go       error classes; depended on by everything, depends on nothing
fs.go           the OS: lock, free space, binary swap, relaunch, decompress, state
fs_unix.go      the OS, unix syscalls
fs_windows.go   the OS, windows syscalls
net.go          HTTP: manifest and signature fetch, artifact download with resume
release.go      release data: semver, manifest, signatures, trust set, rollout
update.go       orchestration: Checker, Guard, Poller
telemetry.go    cross-cutting
```

## What the client expects from the release service

Two documents at fixed URLs, plus one artifact per platform. The client is
configured with the manifest URL and derives everything else.

| Request | Response |
| --- | --- |
| `GET <manifest-url>` | `manifest.json` (below) |
| `GET <manifest-url>.sig` | detached signature: one base64 Ed25519 signature, newline tolerated |
| `GET <artifact url from the manifest>` | the zstd-compressed binary |

The signature URL is *derived* by appending `.sig`, not configured separately —
that removes a way to misconfigure a client into checking release A's manifest
against release B's signature.

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

- `version` must be semver. The client installs only a strictly newer one, so a
  manifest that advertises an older release does not move anybody backwards.
- `rollout` is the cohort percentage, 0–100. **Omitting it means 100** — the
  JSON zero value would otherwise mean "ship to nobody", an invisible failure.
- `platforms` keys are `GOOS-GOARCH`. A release that omits this client's
  platform is "no update", not an error.
- `sha256` and `size` describe the **compressed** artifact: the exact bytes that
  cross the wire.
- Unknown fields are ignored, so the service can add fields (release channels,
  say) without breaking clients that predate them.

What the client sends: `Cache-Control: no-cache` on the manifest, because a CDN
will otherwise happily serve a manifest for a release that has been pulled;
`Accept-Encoding: identity` on the artifact, because the hash covers the
published bytes and transport re-encoding would invalidate it; and a `Range`
request when resuming an interrupted download. A service that ignores `Range`
just gets a restart from zero.

Anything other than HTTPS is refused unless insecure URLs are explicitly opted
into, which is for local testing only. Signatures alone do not fix plaintext: an
attacker who can rewrite responses can suppress updates indefinitely by
replaying a stale but authentically signed manifest.

## The trust model

Each client carries a **trust set** of Ed25519 public keys compiled in at link
time. Keys are never read from a config file, an environment variable, or the
network: an updater that downloads its own trust anchor has no trust anchor.

The client verifies in this order, and nothing is skipped or reordered:

1. **Signature over the manifest**, on the raw bytes as received, before they
   are parsed as JSON. An unsigned manifest is never handed to a parser.
2. **SHA-256 of the artifact**, on the compressed bytes — the ones that crossed
   the wire — before anything is decompressed.
3. **Only then** the swap.

The binary is never executed before it is verified.

A build with an empty trust set refuses to start its updater rather than
accepting unsigned releases, and it fails before touching the network: there is
nothing useful a client can do with a manifest it cannot verify.

### Key rotation, from the client's side

The release service owns the keys. The client's obligation is to trust the
incoming key *before* the service starts signing with it:

1. Add the new public key to the trust set, keeping the outgoing key. Ship it.
2. **Wait for the fleet to pick that build up.** Until a client runs a build
   that trusts the new key, it cannot accept anything signed by it.
3. Only then does the service switch to signing with the new key.
4. Once no supported client trusts the old key alone, drop it.

Skipping step 2 strands every client that has not updated yet: it rejects all
future releases as unsigned and can never update itself out of that state.

The client side of this is `bakedInTrustedKeys` in
[`internal/selfupdate/release.go`](internal/selfupdate/release.go), which carries
the same procedure in its doc comment.

## Integrating it

See [`cmd/app/main.go`](cmd/app/main.go). The library is a struct you fill in;
the part that matters is the ordering:

```go
poller := &selfupdate.Poller{
    Checker:    &selfupdate.Checker{ManifestURL: url, Verifier: verifier, InstallID: id},
    Downloader: &selfupdate.Downloader{},
    StateDir:   stateDir,
}

poller.Startup()      // 1. before anything that could crash
startApplication()    // 2. your real startup
poller.MarkHealthy()  // 3. only after it succeeded
go poller.Run(ctx)    // 4. poll for the life of the process
```

`Startup` runs the crash-loop check: if the previous post-update start never
reported healthy, it restores the retained binary and relaunches. `MarkHealthy`
is what discards that retained binary, so it must come *after* the parts of
startup that can fail — otherwise rollback is defeated by construction.

Build with both linker flags. The version has to be real semver or the client
cannot compare itself against a manifest; the trust set has to be non-empty or
the updater refuses to run:

```sh
go build -ldflags "\
  -X self-update/internal/selfupdate.Version=1.4.2 \
  -X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
```

`TrustedKeysBase64` is a comma-separated list of standard-base64 public keys.

### Staged rollout

Cohort membership is `sha256(installID|version) % 100 < rollout`: deterministic,
so an installation inside the 10% stays inside it as the service raises the
percentage, and each version reshuffles the fleet so the same installations are
not always first. The client decides this locally from the manifest's `rollout`
field — no per-client API call, and nothing for the service to track.

### What happens on an update

Under a `flock` held for the whole cycle, so two instances cannot swap at once:

1. Check free space for the download, the decompressed binary and the retained
   copy, before downloading anything.
2. Download to `<target>.download`, resuming with a `Range` request if
   interrupted, hashing in a single pass as the bytes arrive.
3. Decompress to `<target>.new`, bounded so a bad release cannot fill the disk.
4. Hard-link the running binary to `<target>.old` so it is retained, then
   `rename(2)` the new one over the target. The target is never absent, and a
   crash at any point leaves either the old binary or the new one in place.
5. Write the pending-update marker, then relaunch.

Staging files are cleaned up on every path, including failure.

`ErrRestartRequired` is returned on platforms where the process must exit for its
successor to take over. On unix the relaunch is an `exec`, so it does not return
at all — which is why telemetry is flushed before it.

## Errors and telemetry

Failures are reported as **classes** (`hash_mismatch`, `signature_invalid`,
`disk_full`, `network`, ...), never as raw strings: an error string that reaches a
telemetry endpoint or a log aggregator will contain filesystem paths and
usernames. Signature and hash failures are reported at elevated severity, because
they mean the bytes on the wire did not match what was signed — a compromised
CDN or a MITM, not a flaky hotel network.

Telemetry is fire-and-forget and entirely optional. Leave `Poller.Reporter` nil
and none is sent.

## Tests

**There are none right now.** The previous suite was removed wholesale as the
first step of a restructuring, and its replacement has not landed yet. Until it
does, `go build` and `go vet` are the only things standing between a change and
a shipped bug, which for an updater that replaces its own binary is not much.

What the replacement has to cover, in rough order of how much it would hurt to
get wrong:

- The verification chain: signature over the raw manifest bytes before they are
  parsed, SHA-256 over the compressed artifact before it is decompressed. A test
  that can be satisfied by a client that skips a step is not a test.
- The happy path end to end — download, hash, decompress, swap, relaunch. This
  needs the ability to fabricate a signed release. No tool in this repository
  does that, but `openssl` and `zstd` are sufficient; `CLAUDE.md` has the recipe,
  and the happy path plus the four fail-closed paths have been walked through by
  hand on darwin/arm64 that way. That is evidence, not coverage.
- Rollback attempt accounting, including a marker that is corrupt rather than
  merely present.
- Resumed downloads, and a server that ignores `Range`.

The old suite used real HTTP servers, real zstd, real Ed25519 signatures, real
file swaps and real locks rather than mocks, and one behaviour per test named as
a sentence. That was worth keeping; the replacement should work the same way.
Its release-side fixtures deliberately did not import `selfupdate`, so that a
fixture built with the client's own writers could not agree with the client's own
readers about an encoding the real service does not use. Worth preserving too.

Separately, and unchanged by any of this: two things cannot be verified on a
non-Windows host and need a Windows CI runner — the rename-dance swap with its
`.broken` fallback, and the spawn-and-exit relaunch. `GOOS=windows go vet` is the
only check they currently get.
