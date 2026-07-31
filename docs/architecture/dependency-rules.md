# Dependency and build rules

**When:** Adding an import or a third-party dependency, deciding which file a new helper belongs in, or setting up a build/CI job that produces a client binary.

**Source of truth:** `go.mod`, `internal/selfupdate/doc.go` for the layering rule, the `import` blocks of the nine files under `internal/selfupdate/`, and `release.go` for both ldflag targets. The code wins over this doc.

---

## External dependencies

```
cmd/app  ──▶  internal/selfupdate  ──▶  klauspost/compress/zstd, golang.org/x/sys, stdlib
```

`go.mod`: module `self-update`, `go 1.26.4`, exactly two requirements —
`github.com/klauspost/compress v1.19.1` and `golang.org/x/sys v0.47.0`.

- `zstd` is used only in `fs.go`, by `DecompressFile`, because zstd is what the release
  service publishes: a meaningfully better ratio on Go binaries than gzip, and faster
  decompression on the client, which is the side running on a customer's machine.
- `x/sys` is used only in `fs_unix.go` and `fs_windows.go`, for `flock`/`LockFileEx` and
  `statfs`/`GetDiskFreeSpaceEx`. It must not appear in a file without a build tag.
- Crypto is `crypto/ed25519` and `crypto/sha256` from the standard library, deliberately.
  See [../security/verification-order.md](../security/verification-order.md).

**No new third-party dependency without a reason of that weight.** Both of the existing
ones are load-bearing for a wire format or a syscall that has no portable stdlib
equivalent.

**Nothing imports `cmd/app`.** It is a demo, not a library; anything reusable there belongs
in `selfupdate`.

## The layering rule, now intra-package

With `internal/version` and `internal/releasetest` gone, there is only one internal
package, so the compiler no longer enforces any of this. The rule is the same and now has
to be held by hand. From `doc.go`, ordered bottom to top:

| Layer | Files | May reach for |
| --- | --- | --- |
| 0 | `errors.go`, (`constants.go`, not yet split out) | nothing of ours |
| 1 | `fs.go`, `fs_unix.go`, `fs_windows.go` | layer 0 |
| 2 | `net.go` | layers 0–1 |
| 2 | `release.go` | layer 0 |
| 3 | `update.go` | everything below |
| — | `telemetry.go` | cross-cutting; callable from anywhere, calls nothing above layer 0 |

**Nothing below `update.go` calls back up into it.** That is the rule worth defending, and
it is the one a single package makes easy to break by accident — a helper in `fs.go` can
reference `Poller` and the build stays green.

What breaks if it is violated:

- **The ordering stops being auditable in one place.** `update.go` is where the sequence
  lock → space preflight → download → verify → decompress → swap → mark → relaunch is
  written down, and where the comments explaining why each step precedes the next live. A
  step that reaches back up — `Apply` deciding to write a marker, `Fetch` deciding to
  relaunch — means the real order is spread across files and the next person changing it
  cannot see what they are breaking. The invariants in
  [../update-cycle/invariants.md](../update-cycle/invariants.md) are stated as properties
  of that one sequence; they only hold if the sequence has one home.
- **The lower layers stop being usable standalone.** `Lock`, `Apply`, `DecompressFile`,
  `Relaunch`, `Verifier` and `ensureFreeSpace` are each independently callable, which is
  what makes them checkable in isolation and reusable by an integrator who wants only one
  of them. A dependency on `Poller` turns each into something you can only invoke by
  building the whole orchestrator.
- **The layering would become a cycle if it were ever re-split.** The refactor that
  produced this package merged files; a future one may split them again. Upward references
  are exactly the edges that make that split impossible without untangling them first.

Two corollaries that already hold in the code and should keep holding:

- **`release.go` does not import `net.go`'s concerns and vice versa.** `net.go` calls
  `art.validate()` from `release.go` (reusing the manifest's own rules so a direct caller
  cannot bypass them), which is downward and fine; `release.go` never fetches anything.
- **`runtime.GOOS` branches belong in `fs.go` only where the difference is *layout*, not a
  syscall.** `DefaultStateDir` is the one such branch, because a directory convention is
  not a syscall. Anything that is a syscall goes in the `_unix.go`/`_windows.go` pair. See
  [../platforms/build-tags.md](../platforms/build-tags.md).

## Trust anchors are never a runtime input

No config file, environment variable, or network read may feed the key set. A public key an
attacker can replace reduces signature verification to theatre. Keys are compile-time only,
in `release.go`. See [../security/trust-model.md](../security/trust-model.md) and
[../security/key-management-and-rotation.md](../security/key-management-and-rotation.md).

## The two required build ldflags

Both now target the `selfupdate` package. The old
`-X self-update/internal/version.Version=...` path no longer exists — that package is
gone, and a stale build script using it fails with an unknown-symbol error rather than
silently doing nothing.

```sh
go build -ldflags "\
  -X self-update/internal/selfupdate.Version=1.4.2 \
  -X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
```

`$PUBKEY` is a comma-separated list of standard-base64 Ed25519 public keys. Blank entries
are skipped, so a trailing comma is harmless.

**Without the version flag.** `Version` stays `"0.0.0-dev"`. That *is* valid semver — core
`0.0.0` with prerelease `dev` — so `Checker.Check` proceeds, but it sorts **below every
published release**: the build treats the first manifest it sees as newer and immediately
replaces itself. An unflagged local build pointed at a real release host is not a no-op; it
is an instant update. And if a build injects something that is not semver at all (a git
SHA, a branch name), `Check` fails up front with a `ClassInternal` error and the client can
never update. Both directions are covered in [versioning.md](versioning.md).

**Without the trust-set flag.** `TrustedKeysBase64` is empty, and `bakedInTrustedKeys` — the
in-source half of the trust set — ships with no keys in this repository, so their union is
empty and `TrustedVerifier()` returns an error naming the missing flag. It deliberately does
**not** return a permissive verifier: a build with no keys would otherwise have to choose at
runtime between "reject everything" and "accept anything", and the second is a remote code
execution vector. `cmd/app` treats that error as fatal (exit 1), which makes the mistake
visible at startup rather than on the first check. See
[release-go.md](release-go.md#the-trust-set) and
[../security/fail-closed-points.md](../security/fail-closed-points.md).

Both flags together, or the binary is broken in one of those two directions. A CI job that
builds this client must set both.
