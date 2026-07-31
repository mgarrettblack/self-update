# Artifact Format

**When:** Changing compression or decompression, tuning the decompressed-size bound or the space preflight, or debugging a `decompression` / `hash_mismatch` failure on a downloaded artifact.

**Source of truth:** [`internal/selfupdate/fs.go`](../../internal/selfupdate/fs.go) (`DecompressFile`, `maxDecompressedBytes`, `Apply`, `spaceSafetyMargin`, `ensureFreeSpace`), [`internal/selfupdate/fs_unix.go`](../../internal/selfupdate/fs_unix.go) / [`fs_windows.go`](../../internal/selfupdate/fs_windows.go) (`applySwap`), [`internal/selfupdate/net.go`](../../internal/selfupdate/net.go) (hash over the compressed bytes), [`internal/selfupdate/update.go`](../../internal/selfupdate/update.go) (`Poller.apply`, the preflight and `decompressionRatioEstimate`), design §1, §4. Where this file and the code disagree, the code wins.

---

Artifacts are built and compressed by the release service, which **this repository
does not contain** — there is no in-repo build/compress/publish tool
(`cmd/releaser`, design phase 7, is unimplemented). The client only ever reads
artifacts. There is no reference producer here either: the test-only
`internal/releasetest` package that used to compress fixtures has been removed with
the rest of the suite, so **no code in this repository produces an artifact**, and
the format below is the decoder's requirements read literally.

## Format

- **zstd** (`github.com/klauspost/compress/zstd`), a single stream, no container
  or archive around it. The decompressed output *is* the executable — no tar, no
  zip, no directory structure, one file in and one file out. `DecompressFile`
  opens it with a plain `zstd.NewReader`, so any compression level the service
  chooses decodes fine; the level is the service's decision.
- **One artifact per `GOOS-GOARCH`**, each listed under its own key in
  `platforms` with its own `url`, `sha256` and `size`. See
  [manifest-schema.md](manifest-schema.md).
- zstd rather than gzip for the reason in design §4: a meaningfully better ratio
  on Go binaries, and faster decompression on the client — which is the side that
  matters, because it is the side running on a customer's machine. The service is
  expected to strip debug symbols (`-ldflags="-s -w"`) first; 40–60% total
  shrinkage is typical. The client neither verifies nor cares that stripping
  happened.
- Delta/binary diffs are deliberately out of scope (design §4): Go's linker layout
  shifts too much between builds for diffing to pay off.

## `sha256` and `size` cover the compressed bytes

Both manifest fields describe the artifact **as published** — the exact bytes that
cross the wire — not the binary inside it. That single choice is what makes
verify-before-decompress possible, and it is why `Accept-Encoding: identity` is
set on the artifact request (see [http-endpoints.md](http-endpoints.md)): any
transport re-encoding would change the bytes being hashed and invalidate a digest
that is correct.

`Downloader.Fetch` hashes streaming, in the same pass that writes the file, so
verification costs no second read and the artifact is never held in memory. It
returns nil **only** when the digest matches `art.SHA256`. On a mismatch the file
is deleted: a partial that disagrees with the manifest is not a prefix worth
resuming, and leaving it in place would poison every subsequent attempt forever.
`size` is enforced too — reads are capped at the advertised remaining length, so a
server that streams forever cannot fill the disk while claiming to deliver a few
megabytes — and a resumed download re-hashes the on-disk prefix rather than
trusting it, because a SHA-256 state cannot be persisted.

## What the client does with it

`Poller.apply`, under the update lock, in exactly this order:

1. `Downloader.Fetch(ctx, art, target+".download")` — verified compressed bytes.
2. `DecompressFile(target+".download", target+".new")` — runs **only** on a file
   `Fetch` already verified. Never decompress unverified bytes; a decompressor is
   a parser, and this one is fed by the network.
3. `Apply(target+".new", target)` — retain the previous generation, then
   `rename(2)` over the target.

Both staging files live in the **target's own directory**, because the swap is a
rename and a rename is only atomic within a volume; `Apply` rejects a source
anywhere else rather than degrading to a copy. Both are removed on every exit
path, success or failure, before the lock is released. `Apply` documents "already
verified" as a precondition it does not itself check. Details in
[`../update-cycle/decompress.md`](../update-cycle/decompress.md),
[`../update-cycle/apply-swap.md`](../update-cycle/apply-swap.md) and
[`../update-cycle/locking.md`](../update-cycle/locking.md).

The decompressed file is written by `os.Create` (mode `0o666` before umask) and is
*not* executable when it lands. On unix `applySwap` chmods it to `binaryMode`
(`0o755`) immediately before the rename, or the app would replace itself with
something it cannot run; `restoreOld` re-applies the same mode after a revert.
Windows has no exec bits, so its `applySwap` does the rename dance instead — see
[`../platforms/unix.md`](../platforms/unix.md) and
[`../platforms/windows.md`](../platforms/windows.md).

## The decompression bound

`maxDecompressedBytes` = **1 GiB** (`1 << 30`), applied via
`io.LimitReader(dec, limit+1)` — one byte past the limit, so an artifact that
exactly fills it stays distinguishable from one that overruns.

This is not a decompression-bomb defence in the usual sense, and the comment in
`fs.go` says so: the hash is checked against a signed manifest before anything is
decompressed, so a bomb would have to have come from the release service itself.
The cap earns its place anyway by turning "a bad release fills every user's disk"
into a clean, classified failure, at the cost of one comparison per write.

Every failure path removes `dst`, so a half-written file can never be mistaken for
a complete binary by a later swap. `ClassDecompression` covers a corrupt stream,
an overrun, and an **empty** result (zero bytes decompressed is treated as an
error — an empty file at the target path would be a broken install that the swap
would happily perform).

## Sizing implications for the service

The space preflight runs before the first byte is requested. `Poller.apply`
computes `size * (1 + decompressionRatioEstimate)` with
`decompressionRatioEstimate` = 4 — so **5× the compressed size** — and
`ensureFreeSpace` then adds `spaceSafetyMargin` = 32 MiB on top before comparing
against what the volume actually has available. Both constants are separate:
the multiplier is in `update.go` with the cycle that uses it, the margin is in
`fs.go` with the function that enforces it, and the total requirement is
`5 × size + 32 MiB`.

The estimate errs high on purpose: the compressed `size` is the only figure the
manifest carries, the compressed artifact and the decompressed binary coexist on
disk during the swap, and the previous generation is retained for rollback — via a
hard link where the filesystem allows it, but via a full copy where it does not, so
the second copy has to be budgeted for. If the multiplication overflows, the
declared size is rejected as `ClassManifestInvalid` rather than reaching the
preflight as a negative number. A release whose real expansion ratio exceeds 5×
will still succeed if the disk has room, but it defeats the preflight's
guarantee — running out of space mid-write is precisely the corrupt-binary
scenario the check exists to prevent. See
[`../update-cycle/disk-space.md`](../update-cycle/disk-space.md).
