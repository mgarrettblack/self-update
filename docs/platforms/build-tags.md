# Build Tags and Platform Injection

**When:** Adding a new platform-specific behaviour, editing either half of the `fs_unix.go` / `fs_windows.go` pair, or reaching platform code from the wrong host OS.

**Source of truth:** the `//go:build` lines on `fs_unix.go` and `fs_windows.go`; the `execProcess` and `linkFile` package vars; `Guard.Restore`, `Guard.Now` and `Poller.Relaunch`; `internal/selfupdate/doc.go` for the intended layout. The code wins if they disagree.

---

## One split, not four

`doc.go` states the convention: **`fs.go` wraps the OS, with `fs_unix.go` / `fs_windows.go`
as its platform halves.** A single OS-facing file, not one pair per behaviour.

That is the state on disk. `fs.go` holds locking, free space, the binary swap, relaunch,
decompression and the state directory — everything that touches the OS — and exactly two
tagged files sit under it:

| File | Tag | Provides |
|---|---|---|
| `fs.go` | *(none)* | `AcquireLock`, `Lock.Release`, `ensureFreeSpace`, `OldPath`, `Apply`, `RemoveOld`, `RestoreOld`, `sameDirectory`, `swapError`, `Relaunch`, `DecompressFile`, `InstallID`, `DefaultStateDir` |
| `fs_unix.go` | `!windows` | `lockFile`, `unlockFile`, `freeSpace`, `applySwap`, `restoreOld`, `retainPrevious`, `copyFile`, `linkFile`, `binaryMode`, `execReplace`, `RelaunchReplacesProcess` |
| `fs_windows.go` | `windows` | `lockFile`, `unlockFile`, `freeSpace`, `applySwap`, `restoreOld`, `execSpawn`, `lockRegionLength`, `brokenSuffix`, `RelaunchReplacesProcess` |

Four behaviours are still *split* — locking, free space, the swap, and relaunch — but they
are split across one pair of files rather than four. Earlier revisions of this package had
`apply.go`+`apply_{unix,windows}.go`, `lock.go`+pair, `relaunch.go`+pair and
`space.go`+pair: twelve files where three now do the same work. None of those remain. If
you find a reference to `apply_unix.go` or `space_windows.go` anywhere, it is stale.

The consolidation is not cosmetic. The four behaviours are not independent — the swap
decides what `RemoveOld` has to clean up, and *that* differs by platform because the
relaunch differs by platform (see [windows.md](windows.md)). Keeping them in one file
means the coupling is visible in one place instead of implied across four pairs that each
looked self-contained.

One caveat on `doc.go`: it also names a `constants.go` at the bottom of the dependency
order. That file does not exist. Constants live next to the code that uses them
(`spaceSafetyMargin` and `maxDecompressedBytes` in `fs.go`, `lockRegionLength` in
`fs_windows.go`, the poll and staging constants in `update.go`). Treat the `constants.go`
mention as aspirational, not as a file you failed to find.

## The conventions

- **Tag by negation, not by enumeration.** `!windows` rather than
  `linux || darwin || freebsd`. Unix variants are the default and a new Unix-ish GOOS
  should build without touching the tags. Windows is the exception, named explicitly.
- **The logic lives in the neutral file; the halves hold primitives.** `Apply`,
  `AcquireLock`, `Relaunch`, `ensureFreeSpace` and `DecompressFile` are each defined once
  in `fs.go`, with all validation, path handling, classification and ordering there. The
  tagged half is the smallest possible primitive: `applySwap` does the swap and nothing
  else; `freeSpace` returns a number. Everything a reader has to reason about should be
  readable on any host.
- **Classification stays out of the halves.** `fs_unix.go` contains no `ErrorClass`
  reference at all: its failures go through `swapError`, which is in `fs.go`.
  `fs_windows.go` reaches for `classifyf(ClassSwapFailed, ...)` only for its two
  double-failure cases, which have no neutral equivalent to route through. This is why
  [../observability/error-classes.md](../observability/error-classes.md) can map classes
  to files without a per-platform column.
- **Same signature on both sides.** A split function that differs in shape between the two
  files puts the difference into the callers, which then need tags of their own.
- **One deliberate exception to "exported API in the neutral file":**
  `RelaunchReplacesProcess` is an exported `const` declared in *both* halves — `true` in
  `fs_unix.go`, `false` in `fs_windows.go`. Its value *is* the platform difference, so
  there is nothing to hoist. Callers need it because whether `Relaunch` returns on success
  is not knowable from the signature. See [../update-cycle/relaunch.md](../update-cycle/relaunch.md).

Note that `//go:build windows` code is not compiled at all on a Mac or Linux dev machine.
Plain `go build ./...` and `go vet ./...` never look at it. `GOOS=windows go build ./...`
does compile it, and currently succeeds — that is the one cheap check available, and it is
worth running after any edit to `fs_windows.go`. It proves the file compiles and nothing
else; [known-gaps.md](known-gaps.md) is where that runs out.

## Injection: the seams, and what they are actually worth now

Platform behaviour is reached through an indirection rather than called directly. Every
seam below exists in the code today:

**Package vars** — for a primitive with exactly one real implementation per platform:

```go
// fs.go
var execProcess func(path string, argv, env []string) error  // set by init() in each half

// fs_unix.go
var linkFile = os.Link
```

`execProcess` is set by `init()` in `fs_unix.go` (`execReplace`, i.e. `syscall.Exec`) and
in `fs_windows.go` (`execSpawn`). `Relaunch` in `fs.go` calls it through the var, and does
everything else itself: absolute-path resolution, the stat preflight, the argv copy and
`argv[0]` defaulting, and the error classification. It also handles `execProcess == nil`
explicitly with a `ClassInternal` error, which is the only way a platform with no
registered primitive could surface.

`linkFile` is the seam under `retainPrevious`'s copy fallback: the fallback only triggers
on a filesystem that refuses hard links, and pointing `linkFile` at a stub that returns an
error reaches it on any filesystem.

**Struct fields** — for behaviour a caller may legitimately want to override:

```go
Guard.Restore   func(target string) error                // nil means RestoreOld
Guard.Now       func() time.Time                         // nil means time.Now
Poller.Relaunch func(path string, argv []string) error    // nil means Relaunch
```

All three are zero-value-meaningful: nil falls back to the real implementation, matching
the package's convention of optional fields over constructors rather than adding a
constructor to set them. `Guard.Restore` is the seam under the whole attempt-accounting
and revert path in `update.go`, which can then run without a real binary swap.
`Poller.Relaunch` is the seam that lets a full update cycle reach completion without the
current process exec'ing itself — and it is also the reason `ErrRestartRequired` is
reachable on a unix host at all, since the real unix `Relaunch` never returns to set
`RestartPending`.

### Be honest about why these are here

The seams are the right structure and they should stay. But the argument that used to
justify them — that they make the platform-adjacent logic unit-testable on any host — is
now only half true, because **there are no tests in this repository.** Not zero Windows
tests: zero tests. `internal/releasetest` and every `*_test.go` file are gone. See
[known-gaps.md](known-gaps.md).

So state the value accurately:

- What still holds is that the seams keep the *logic* out of the tagged files. Reading
  `Relaunch` in `fs.go` tells you what happens on both platforms; that benefit is
  independent of anyone testing it, and it is why a reviewer on a Mac can review the
  Windows relaunch path meaningfully.
- What still holds is that the seams are the only mechanism by which this logic *could* be
  exercised from a host that cannot run the primitive. They are a precondition for
  coverage.
- What does **not** hold is any claim that the logic *is* covered. Nothing verifies it.
  `Poller.Relaunch`'s field comment says "for tests" and `linkFile`'s says the fallback
  "can be exercised in tests"; both describe an intent, not a fact, and the tests they
  refer to no longer exist.

Keep adding the seams. Do not cite them as evidence that anything works.

## If you are adding a split

1. Put the logic in `fs.go` and the syscall in the pair. If you find yourself duplicating
   a validation check in both halves, it belongs in the neutral file.
2. Give the primitive an indirection — a package var set from `init()`, or a
   zero-value-meaningful struct field on whatever type owns the operation.
3. Keep the primitive's signature identical in both halves, and route its failures through
   a neutral classifier (`swapError`, or a new equivalent) rather than tagging classes
   inside the tagged file.
4. Compile both sides before you are done: `go build ./...` and `GOOS=windows go build ./...`.
   The second is not optional — nothing else in this repo will ever look at the Windows
   half.

**Skipping step 2 is not a style problem.** A platform primitive with no injection point
has no reachable caller-side coverage from the machines that build this repo, now or if a
test suite is reintroduced later. Right now nothing is covered either way, which makes the
seam cheaper to add and easier to forget — add it anyway, because retrofitting one across
a call graph is far more expensive than declaring a var.

The platform-specific reasoning itself is in [unix.md](unix.md) and
[windows.md](windows.md). For where these calls sit in the package's dependency order, see
[../architecture/dependency-rules.md](../architecture/dependency-rules.md).
