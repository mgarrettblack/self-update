# Decompressing the artifact

**When:** Editing `DecompressFile` in `internal/selfupdate/fs.go`, changing the compression format, or debugging a `decompression`-class failure or a `<target>.new` file that is missing / the wrong size.

**Source of truth:** `internal/selfupdate/fs.go` (`DecompressFile`), `internal/selfupdate/constants.go` (`maxDecompressedBytes`), `internal/selfupdate/update.go` (`Poller.apply`), `self-update-design.md` §1, §4. The code wins if this document disagrees with it.

---

```go
DecompressFile(src, dst string) error   // src = <target>.download, dst = <target>.new
```

One exported function, with the output bound taken from the package constant
`maxDecompressedBytes` (in `constants.go`) rather than a parameter. Both staging paths sit in the
target's own directory — see [invariant 4](invariants.md).

## The precondition

`DecompressFile` only ever runs on a file `Downloader.Fetch` has already returned
nil for. `Poller.apply` enforces this by sequence:

```go
if err := downloader.Fetch(ctx, d.Artifact, compressed); err != nil {
    return err
}
// Only now, on bytes that are already verified.
if err := DecompressFile(compressed, staged); err != nil {
    return err
}
```

This is [invariant 2](invariants.md). The SHA-256 in the signed manifest covers
the *compressed* bytes — the ones that crossed the wire (§1) — so the compressed
file is the last point at which anything can be checked against the manifest at
all. There is no digest for the decompressed output, and there cannot be one
without changing the release contract.

Any new call site of `DecompressFile` has to be able to point at the verification
immediately preceding it. The function does not check anything itself.

## Why zstd

Artifacts are compressed by the release service, which publishes zstd. §4's
reasoning: a meaningfully better ratio on Go binaries than gzip, and faster
decompression on the *client* — which is the side that matters, because it is the
side running on a customer's machine. `github.com/klauspost/compress/zstd` is the
implementation, and it is one of only two non-stdlib dependencies the module has
(the other is `golang.org/x/sys`).

Combined with `-ldflags="-s -w"` on the release build, §4 expects Go binaries to
shrink 40–60%.

Do not add transport-level compression on top: see
[`Accept-Encoding: identity`](download-and-resume.md).

## The output bound

```go
const maxDecompressedBytes = 1 << 30 // 1 GiB
```

The honest framing, from the constant's comment in `constants.go`: the artifact's hash is checked against a
signed manifest before it is ever decompressed, so a decompression bomb would have
to have come from the release service itself. This is not a defence against an
external attacker — the signature already is.

The cap is still worth having. It turns "a bad release fills every user's disk"
into a clean, classified `ClassDecompression` failure, and it costs one comparison
per write. A release pipeline that accidentally ships a sparse or pathological
artifact is a far more likely event than a compromised signing key, and the
difference between the two outcomes is whether the user's machine is still usable
afterwards.

`io.LimitReader(dec.IOReadCloser(), maxDecompressedBytes+1)` reads one byte past
the limit so an artifact that exactly fills it is still distinguishable from one
that overruns — the same technique as the manifest cap in [check](check.md).

Note this bound is *independent* of the disk-space preflight, which sizes the write
using `decompressionRatioEstimate` from the manifest's compressed size. The
preflight asks "will this fit"; the cap asks "is this plausible at all". See
[disk space](disk-space.md).

## Failure cleanup

Every failure path removes `dst`:

| Condition | Class |
|---|---|
| cannot open `src` | `ClassOf(err)` |
| not a valid zstd stream / truncated stream | `ClassDecompression` |
| cannot create `dst` | `ClassOf(err)` |
| copy error mid-stream | `ClassDecompression` |
| output exceeds the limit | `ClassDecompression` |
| close error (a deferred write failing) | `ClassOf(err)` |
| output is zero bytes | `ClassDecompression` |

`dst` is removed so a half-written file can never be mistaken for a complete
binary by a later swap. `Poller.apply` also unconditionally removes both staging
files on every exit path, under the lock, so there are two layers here — but
`DecompressFile` is exported and cleans up after itself regardless of caller.

The zero-byte case is called out separately because an empty output is a valid
zstd stream of an empty input, and `Apply` would happily swap a zero-byte file
onto the target. That would leave the install with an executable path that exists,
is executable, and cannot run — the worst kind of failure, because it looks
successful.

The `written == 0` and `written > maxDecompressedBytes` checks are only reachable
when `copyErr` is nil, so the `switch` order matters: a genuine stream error is
reported as itself rather than as an empty-artifact error.

## Related

- [download and resume](download-and-resume.md) — the step that must precede this
- [apply and swap](apply-swap.md) — the step that consumes `<target>.new`
- [error classes](../observability/error-classes.md)
