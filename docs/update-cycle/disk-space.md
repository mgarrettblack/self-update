# The disk-space preflight

**When:** Editing `ensureFreeSpace` in `internal/selfupdate/fs.go` or `freeSpace` in `fs_unix.go` / `fs_windows.go`, changing `spaceSafetyMargin` or `decompressionRatioEstimate`, or debugging a `disk_full` failure that fired on a disk that looked fine.

**Source of truth:** `internal/selfupdate/fs.go` (`ensureFreeSpace`), `internal/selfupdate/fs_unix.go` and `fs_windows.go` (`freeSpace`), `internal/selfupdate/update.go` (`Poller.apply`), `internal/selfupdate/constants.go` (`spaceSafetyMargin`, `decompressionRatioEstimate`), `self-update-design.md` §2. The code wins if this document disagrees with it.

---

```go
func ensureFreeSpace(dir string, needBytes int64) error   // ClassDiskFull if short
func freeSpace(dir string) (uint64, error)                // per-platform
```

Both are unexported: the preflight is part of the cycle, not part of the package's
API. `Poller.apply` is the only caller, and it runs **before the first byte is
requested** — after the lock is taken, before `Fetch`.

## Why before, not during

§2: an out-of-space failure mid-write is exactly the "corrupt binary" scenario
atomicity is meant to prevent — but only if it is caught before writing starts, not
after. Once a write is partway through there is no recovery that does not involve
deleting something, and the failure surfaces as an `ENOSPC` from whichever step
happened to be unlucky rather than as the disk problem it is.

## What is sized

`Poller.apply` computes the requirement itself and hands it over:

```go
need := d.Artifact.Size * (1 + decompressionRatioEstimate)   // decompressionRatioEstimate = 4
if need < d.Artifact.Size { // overflow on an absurd declared size
    return classifyf(ClassManifestInvalid, "update",
        "declared artifact size %d bytes is not plausible", d.Artifact.Size)
}
if err := ensureFreeSpace(dir, need); err != nil { ... }
```

`Decision.Artifact.Size` is the *compressed* size, which is the only figure the
signed manifest carries. There is no decompressed size in the manifest, so the
client has to estimate, and the estimate errs high:

> The compressed size is the only figure the manifest carries, and running out of
> space halfway through decompression is exactly the failure the preflight exists to
> prevent, so the estimate errs high.

A ratio of 4 against §4's expected 40–60% shrink on Go binaries is roughly double
the realistic expansion. Overestimating costs an update deferred on a nearly-full
disk; underestimating costs a failed decompression on a disk that the preflight
just declared healthy.

The `1 +` accounts for the compressed artifact and the decompressed binary
coexisting on disk during the swap — they do, because `<target>.download` and
`<target>.new` are both live until `Poller.apply`'s deferred cleanup runs.

The overflow guard sits at the call site because that is where the multiplication
is, and it returns `ClassManifestInvalid`, not `ClassDiskFull`: a declared size
large enough to wrap `int64` means the manifest is wrong, not that the disk is
full. `ensureFreeSpace` carries its own overflow check for the addition of the
margin, classified `ClassInternal` for the same reason.

The retained `.old` copy is covered by the safety margin rather than by a third
term. On unix retention is normally a hard link, which costs no data blocks at all;
the margin is what covers the `copyFile` fallback in `retainPrevious`, which its
comment explicitly relies on — "the disk-space preflight already accounted for a
second copy". See [apply and swap](apply-swap.md).

## The safety margin

```go
const spaceSafetyMargin = 32 << 20 // 32 MiB
```

Checking for exactly `needBytes` would be wrong in both directions, and the
constant's comment in `constants.go` enumerates why:

- The download is not the only thing that needs room: the compressed artifact and
  the decompressed binary coexist during the swap, and the previous binary is
  retained as `.old` for one generation of rollback (§3).
- Filesystem metadata, block-size rounding, and copy-on-write snapshots all consume
  more than the logical byte count. On a CoW filesystem the "free" number can be
  claimed by a snapshot the moment you write to a block.
- **Free space is a moving target.** Other processes are writing while the update
  runs, so a check that passes with zero slack can still hit `ENOSPC` seconds later
  — precisely the mid-write corruption this preflight exists to prevent.

32 MiB is small enough never to block an update on a healthy disk and large enough
to absorb that noise for binaries of the size Go produces.

Note where that rationale lives: the tunable moved to `constants.go` and its
argument moved with it, so a reader of `ensureFreeSpace` no longer sees the case
for 32 MiB at the point of use. `constants.go` says as much about itself — that is
the acknowledged price of collecting the tunables, and dropping the argument was
never an option.

## Which directory

`Poller.apply` passes `filepath.Dir(target)`.

`ensureFreeSpace`'s doc comment is explicit: `dir` must be the directory the
download will actually be written to — the directory holding the running executable
— **not a temp directory elsewhere**. The swap requires the staged file to be on the
same volume as its target ([invariant 4](invariants.md)), so that is the only
filesystem whose free space is relevant. Checking `os.TempDir()` would measure a
volume nothing is written to.

## Available, not unused

Both platform implementations report what *this process* may write, which is
narrower than "unused":

- **unix** (`fs_unix.go`) uses `Statfs.Bavail`, **not** `Bfree`. `Bfree` counts
  every unallocated block, including the percentage the filesystem reserves for root
  (5% by default on ext4, similar elsewhere). An ordinary user process cannot use
  reserved blocks, so counting them would let the preflight pass and the write still
  fail with `ENOSPC` — the exact outcome the check exists to prevent.
- **Windows** (`fs_windows.go`) uses `GetDiskFreeSpaceEx`'s
  `freeBytesAvailableToCaller`, **not** `totalNumberOfFreeBytes`, because the former
  honours per-user disk quotas. On a quota-managed machine the volume can have
  gigabytes free while this user is allowed none of it.

Field widths and signedness of `Bavail`/`Bsize` vary by platform, hence the explicit
`uint64` conversions in the unix implementation.

## Failure modes are classified apart

| Condition | Class | Why |
|---|---|---|
| genuinely short of space | `ClassDiskFull` | the actionable case |
| `needBytes <= 0` | `ClassInternal` | a manifest with a missing or bogus `size`, or an arithmetic slip — deliberately not `disk_full` so it cannot be mistaken in telemetry for a fleet of full disks |
| `needBytes` + margin overflows `uint64` | `ClassInternal` | same reasoning |
| empty `dir` | `ClassInternal` | caller bug |
| `statfs` / `GetDiskFreeSpaceEx` fails | `ClassOf(err)` | **not** treated as "probably fine": proceeding on an unknown amount of space defeats the point of the check |

That last row is the one worth internalising. A stat failure is returned as an
error, not swallowed, because a preflight that passes when it could not measure
anything is worse than no preflight — it reports confidence it does not have.

The `ClassDiskFull` message includes the breakdown (`need N bytes (X artifact + Y
margin), Z available`) so a field report is diagnosable without reproducing it.

## Related

- [download and resume](download-and-resume.md) — the step this protects
- [decompress](decompress.md) — the 1 GiB output cap, a different bound with a different job
- [error classes](../observability/error-classes.md)
- [platform splits](../platforms/build-tags.md)
