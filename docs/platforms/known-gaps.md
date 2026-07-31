# Known Gaps

**When:** Before relying on any claim that this code has been verified — in particular before trusting Windows swap/relaunch behaviour, before producing or consuming a real signed release, or when estimating how much confidence the repo actually supports.

**Source of truth:** the absence of any `*_test.go` file in the repo, the absence of CI configuration, the absence of `cmd/releaser` and `cmd/devserver`, the `//go:build windows` half of `internal/selfupdate/fs.go`, `internal/selfupdate/doc.go` for the intended layout, and the phased plan at the end of `self-update-design.md` (phases 3 and 7). The code wins if they disagree.

---

This file exists so a future reader does not assume coverage that is not there. Everything
below is a statement about *verification*, not about believed correctness. The code may
well be right. Nothing here demonstrates that it is.

Read this section first, because it changes how you should read every other document in
`docs/`.

## 1. There are no tests in this repository

Not "no Windows tests." No tests.

- `find . -name '*_test.go'` returns nothing.
- `internal/releasetest` — the test-only package that fabricated keypairs, detached
  signatures and zstd artifacts, and that deliberately did not import `internal/selfupdate`
  so fixtures could not agree with the client about an encoding the real service does not
  use — has been deleted.
- `go test ./...` therefore reports no test files and passes trivially. **A green
  `go test` in this repo means nothing at all.**

This invalidates a class of claim that appears in older notes and in some source comments:
that a behaviour is "covered", "tested", or "exercised". It is not. Where you find such a
claim, treat it as describing an intention.

Specific source comments that are stale in this direction, and that are left in place
deliberately rather than quietly edited:

- `linkFile` in `fs_unix.go` — "indirected so the copy fallback in `retainPrevious` can be
  exercised in tests without needing a filesystem that refuses hard links."
- `Poller.Relaunch` in `update.go` — "replaces the platform's relaunch, for tests."
- `Reporter.Wait` in `telemetry.go` — "For tests and for a clean shutdown only — the update
  path never calls it." This one is doubly wrong: `Wait` is called on the update path, in
  both `Poller.Update` and `Poller.Startup`. See
  [../observability/telemetry-events.md](../observability/telemetry-events.md).
- `backoffDelay` in `net.go` — "keeps the function usable from tests."

The seams those comments describe do all still exist, and they are still the right
structure — see [build-tags.md](build-tags.md). They are simply unexercised.

## 2. There is no CI

No `.github/` directory, no `Makefile`, no `Taskfile`, no `.gitlab-ci.yml`, no runner
configuration of any kind in the project tree. Nothing builds, vets, cross-compiles or
tests this code except a developer typing the command.

The practical consequence compounds with §1: there is no automated environment in which
`//go:build windows` is satisfied, and no automated environment in which anything at all is
checked. The commands that *do* work, and are worth running by hand before finishing any
change:

```sh
go build ./...                 # host platform
go vet ./...                   # host platform only — never looks at fs_windows.go
GOOS=windows go build ./...    # the only automated-ish check the Windows half ever gets
```

As of this writing all three pass cleanly. That is a real (if minimal) fact and worth
preserving: it means the tree compiles on both platform halves, which older notes claiming
a broken build no longer describe. It also means `GOOS=windows go build` is the single
cheapest regression check available for `fs_windows.go`, and skipping it is how that file
rots silently.

## 3. The Windows half has never been run

`fs_windows.go` is `//go:build windows`. It compiles under `GOOS=windows` and has been
executed by nothing.

Unverified against a real Windows filesystem or process:

- The **rename dance** in `applySwap` — that Windows actually permits `os.Rename` on the
  running `.exe` because the loader opened it with `FILE_SHARE_DELETE`. The design says the
  loader "typically" does this (§2), and the entire swap is built on that permission. It is
  a well-documented behaviour, not one measured here.
- The **put-back on failure** path, where the third rename fails and `.old` is renamed
  straight back, and the double-failure case where the put-back fails too.
- The **`.broken` fallback** in `restoreOld`, including the case its comments predict —
  that `os.Remove(broken)` fails while the current process is still executing that image
  and gets cleaned up on a later pass. Whether the *later pass* reliably succeeds has never
  been observed.
- That `RemoveOld`, called from `Poller.MarkHealthy`, actually succeeds in deleting
  `app.exe.old` once the outgoing process is gone.
- That `exec.Cmd` with `CREATE_NEW_PROCESS_GROUP` and inherited standard streams gives a
  CLI successor a usable console across an update, and that `cmd.Process.Release()`
  followed by the parent exiting leaves the successor healthy and unparented.
- The full handoff: successor starts, outgoing process exits, `app.exe.old` becomes
  deletable.

`lockFile`/`unlockFile` and `freeSpace` in the same file are in the same position, and are
less alarming only because they are smaller. Note how they would fail, though: a wrong
`LockFileEx` region or a wrong `GetDiskFreeSpaceEx` field would each fail *silently in the
safe-looking direction* — no lock actually held, or an over-optimistic free-space number —
so neither would announce itself. See [windows.md](windows.md) for what each is supposed
to do.

**What would close this:** a Windows runner. The design's own phase 3 says the Windows swap
and relaunch "needs a Windows CI runner to trust it." No such runner exists here, so phase
3 is incomplete regardless of the code having been written.

The unix half is in a better position only in that it runs on the development platform at
all — the same code paths execute during any manual run of `cmd/app`. That is exercise, not
verification.

## 4. There is no in-repo way to produce a real signed release

`cmd/` contains only `app`. The design's repo layout lists two more binaries that are
**not implemented** (phase 7, "release pipeline glue"):

- `cmd/releaser` — the CI tool that builds, strips, compresses, hashes and signs the
  manifest.
- `cmd/devserver` — a trivial static file server for local testing.

Consequences:

- The release side of the contract — key generation, building, compressing, signing,
  hosting — is a separate service this repo does not own. The client's expectations of it
  are documented and not executed. See
  [../release-contract/manifest-schema.md](../release-contract/manifest-schema.md),
  [../release-contract/signature-format.md](../release-contract/signature-format.md),
  [../release-contract/artifact-format.md](../release-contract/artifact-format.md) and
  [../release-contract/http-endpoints.md](../release-contract/http-endpoints.md).
- **No artifact has ever been end-to-end verified by this client** — not by a real
  pipeline, and, since `internal/releasetest` was deleted, not by a fabricated one either.
  A mismatch in signature encoding or compression framing would not be caught anywhere in
  this repo. Older notes describing fixture-based end-to-end coverage no longer apply.
- `bakedInTrustedKeys` in `release.go` is empty — no keys are committed. Every build must
  supply a trust set via `-ldflags`, and `TrustedVerifier` fails closed when none is
  present. See [../security/key-management-and-rotation.md](../security/key-management-and-rotation.md)
  and [../security/fail-closed-points.md](../security/fail-closed-points.md).
- Building requires **both** `-X` settings, and both now target `internal/selfupdate`:

  ```sh
  go build -ldflags "\
    -X self-update/internal/selfupdate.Version=1.4.2 \
    -X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
  ```

  Without them the binary either never sees a newer version (`Version` defaults to
  `0.0.0-dev`, valid semver but never newer) or refuses to start its updater at all (empty
  trust set). A build that "works" without them has not exercised the updater. Watch for
  older instructions naming `self-update/internal/version.Version` — that package no longer
  exists, and the flag would be silently ignored, which looks exactly like a working build
  that never updates.
- `once`, `insecure`, `confirm` and `state_dir` in `cmd/app`'s config file exist to drive
  the client against a local release host you must supply yourself. See
  [../architecture/cmd-app.md](../architecture/cmd-app.md).

## 5. `doc.go`'s layout is nearly, but not entirely, realized

`doc.go` describes the target file layout, and the consolidation it describes has largely
happened: `fs.go` with `fs_unix.go`/`fs_windows.go` halves, `net.go` above it, `release.go`
above that, `update.go` as the orchestration layer. The former `internal/version`,
`keys.go`, `verify.go`, `manifest.go`, `rollout.go`, `download.go`, and the four
`_unix`/`_windows` pairs are all gone.

What has not landed:

- **`constants.go` does not exist.** `doc.go` places it at the bottom of the dependency
  order alongside `errors.go`. Constants currently live next to their users
  (`spaceSafetyMargin`, `maxDecompressedBytes`, `lockFileMode`, `oldSuffix` in `fs.go`;
  `lockRegionLength`, `brokenSuffix` in `fs_windows.go`; the poll, staging and backoff
  constants in `update.go` and `net.go`). Either the file is still to be created or the
  doc comment should stop naming it; right now a reader looking for it will not find it.
- `internal/selfupdate/rollback.md` sits inside the package as a Markdown design note, and
  `update.go` references it from doc comments on `Guard` and `CheckStartup`. It is not part
  of the layout `doc.go` describes and is not linked from `docs/`. It duplicates some of
  [../update-cycle/rollback.md](../update-cycle/rollback.md), and the two can drift.

The dependency direction `doc.go` asserts — strictly downward, nothing below `update.go`
calling back up — does hold in the current tree. See
[../architecture/dependency-rules.md](../architecture/dependency-rules.md).

## 6. Not gaps

For contrast, so these are not mistaken for holes:

- The tree **does** compile, on both platform halves. Notes claiming otherwise are stale.
- `Marker` survival, rather than a wall-clock window, is the crash-loop signal. That is a
  deliberate and documented divergence from §3's phrasing — not an unimplemented
  requirement. See [../update-cycle/rollback.md](../update-cycle/rollback.md).
- The seams (`execProcess`, `linkFile`, `Guard.Restore`, `Guard.Now`, `Poller.Relaunch`)
  are all present and are the right structure. They are unexercised, which is a coverage
  gap, not a design gap. See [build-tags.md](build-tags.md).
- Telemetry being entirely optional, and every telemetry failure being swallowed, is
  intended behaviour rather than missing error handling (§5). See
  [../observability/telemetry-events.md](../observability/telemetry-events.md).
