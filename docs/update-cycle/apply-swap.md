# Applying the swap

**When:** Editing `Apply` in `internal/selfupdate/fs.go` or `applySwap` in `fs_unix.go`, adding a call site for `Apply`, or debugging a `swap_failed` / `permission_denied` failure or a stray `.old` file.

**Source of truth:** `internal/selfupdate/fs.go` (`Apply`, `OldPath`, `RemoveOld`, `RestoreOld`, `sameDirectory`, `swapError`), `internal/selfupdate/fs_unix.go` (`applySwap`, `retainPrevious`, `copyFile`, `binaryMode`), `internal/selfupdate/fs_windows.go`, `internal/selfupdate/constants.go` (`oldSuffix`), `self-update-design.md` §2, §3, §6. The code wins if this document disagrees with it.

---

```go
Apply(newBinary, target string) error
```

Replaces `target` with `newBinary`, retaining the previous binary at
`OldPath(target)` — `target + ".old"` — for one generation so rollback is
possible (§3). One generation is kept: enough for a single rollback, without
accumulating stale binaries next to the live one.

## The two documented preconditions

`Apply` checks one of them and deliberately not the other.

**1. `newBinary` MUST already be fully verified.** Hash *and* signature, before
`Apply` is called. Nothing in `fs.go` re-checks a digest — this is
[invariant 3](invariants.md), and the guarantee lives entirely in
`Poller.apply`'s ordering. §1 forbids ever touching the live executable path with
unverified bytes. A new call site of `Apply` must be able to point at the
verification that precedes it.

**2. `newBinary` MUST live in the same directory as `target`.** This one *is*
enforced, and enforced by rejection rather than by falling back to a copy:

```go
if !sameDir {
    return classifyf(ClassSwapFailed, "apply",
        "staged binary %q is not in the target's directory %q: a cross-volume rename is not atomic",
        newBinary, filepath.Dir(target))
}
```

Same directory is the only portable way to guarantee same volume. A cross-volume
rename is not a rename at all — the OS silently degrades it to a copy, which is not
atomic and can leave a truncated binary at the target path if the process dies
mid-write (§2). Papering over that with a copy of our own would hide the loss of
atomicity from the caller, so `Apply` returns `ClassSwapFailed` and the caller
fixes *where it staged the download*.

`sameDirectory` compares absolute directory paths, and on mismatch retries with
symlinks resolved, so `/var/folders/...` and `/private/var/folders/...` on macOS
are not mistaken for different volumes. If either directory cannot be resolved,
the textual comparison stands as final.

## The unix swap

`applySwap` in `fs_unix.go`, three steps:

1. `chmod` the staged binary to `binaryMode` (`0o755`),
2. retain the current binary at `target + ".old"` via a **hard link**,
3. `rename(2)` the staged binary over `target`.

The chmod is not optional: the staged file lands as a plain `0o600`/`0o644` file —
the download is written at `privateFileMode` and the decompressed output takes
whatever `os.Create` gives it — so the exec bits have to be added before the rename
or the app replaces itself with something it cannot run.

### Why renaming over a running executable is safe

A process does not execute "the file at this path". It holds the inode open via a
file descriptor, and the kernel keeps that inode and its pages alive as long as the
descriptor exists. `rename(2)` only repoints the directory entry, so the running
process keeps executing the old image undisturbed while every new exec of that path
gets the new one (§2). This is why unix needs no rename dance — contrast
[the Windows swap](../platforms/windows.md).

### Why the previous generation is a hard link, not a rename

**So `target` is never, even briefly, absent.** `rename(2)` of the new file over
the still-linked old one is a single atomic step: a crash at any instant leaves
either the complete old binary or the complete new one at `target`. Renaming
`target` to `.old` first would open a window where the app's path does not exist at
all — and anything that looks the path up during that window (a shell, a
supervisor, a cron entry, a user typing the command) fails.

`retainPrevious` removes a stale `.old` first, because `os.Link` fails with
`EEXIST`. If `target` does not exist at all, that is a **first install rather than
an update**: nothing is retained, and any stale `.old` is dropped so a later
rollback cannot resurrect a binary from an unrelated generation.

If the filesystem refuses a hard link — some FUSE mounts, overlayfs upper layers,
and network filesystems do — `retainPrevious` falls back to `copyFile`. Retention
is what makes rollback possible, so a copy is the right trade: slower and briefly
uses twice the space, but the disk-space preflight already accounted for a second
copy. `copyFile` syncs before close so the retained generation is durable rather
than sitting in the page cache when the machine loses power mid-update. `linkFile`
is a package var rather than a direct `os.Link` call, so that fallback can be
driven on a host whose filesystem happily supports links.

## `RemoveOld` and `RestoreOld`

`RemoveOld(target)` deletes the retained generation, best effort — a missing file
is not an error. It is called from `Poller.MarkHealthy`, once the new binary has
proven itself. On Windows it is also the deferred cleanup for the rename dance,
which cannot delete the outgoing `.exe` while the old process still holds it.

`RestoreOld(target)` reverts. It fails with `ClassSwapFailed` if nothing was
retained, since there is then nothing to roll back to. Its important caveat:

> After a successful `RestoreOld` the process is still running the *new* image
> while the *old* one sits at `target`: the caller must `Relaunch` (or exit and let
> its supervisor restart it) for the revert to take effect.

`Guard.Restore` defaults to `RestoreOld` and is a struct field so the revert path
can be exercised without a real `.old` binary on disk. See [rollback](rollback.md).

## Error classification

`swapError` gives permission denial its own class:

```go
if ClassOf(err) == ClassPermissionDenied {
    return classify(ClassPermissionDenied, op, err)
}
return classify(ClassSwapFailed, op, err)
```

Permission denial is the expected outcome of installing into a directory the
process does not own — §6's locked-down corporate machine — and it is actionable
in a way that a generic swap failure is not.

Note what the package does *not* do about that: it provides no install-location
helper. `DefaultStateDir` chooses where *state* lives, but the binary's own
location is whatever `Poller.TargetPath` names, or the running executable with
symlinks resolved. Keeping the install per-user — rather than `/usr/local/bin` or
`Program Files` — is the deployment's responsibility, and the design calls for it
in the Assumptions table and §6 precisely because a system directory turns every
self-update into a `permission_denied`.

## Related

- [invariants](invariants.md)
- [relaunch](relaunch.md) — the step after the marker
- [rollback](rollback.md) — the consumer of `.old`
- [Windows platform notes](../platforms/windows.md) — the rename dance and `.broken`
- [unix platform notes](../platforms/unix.md) — rename-over-running-executable
