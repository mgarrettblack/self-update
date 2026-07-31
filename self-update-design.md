# Self-Updating Application — System Design

## Problem

A deployed Go application needs to seamlessly update itself in the field, across Windows, macOS, and Linux, without a heavyweight server component.

## Assumptions

| Question | Assumption |
|---|---|
| Update trigger | Check on startup, then every hour on a jittered timer. Not push-based — keeps the server dumb. |
| User approval | Silent auto-update by default. Config flag available to require confirmation for enterprise/regulated environments. |
| Server complexity | Static file hosting only (S3/CloudFront, GCS, or GitHub Releases). No custom backend logic for distributing binaries. A minimal reference server is provided for local dev/testing only. |
| Delta/binary diffs | Out of scope. Go's linker layout shifts too much per build for diffing to reliably pay off; full-binary download + compression is the better lever. |
| Release channels | Single channel for v1. Manifest schema leaves room for `stable`/`beta` via URL path later. |
| Auth on update endpoint | None. It's public and read-only; integrity/authenticity is enforced via signature, not access control. |
| Multiple running instances | Possible. A file lock ensures only one instance performs the actual swap; others pick up the new binary on their own next restart. |
| Install location | Per-user directory by default (`~/.local/bin`, `%LOCALAPPDATA%`), to avoid needing elevated permissions to self-replace. |

---

## 1. Security — don't download or run a malicious file

- **Transport**: HTTPS only, standard system cert validation. Necessary but not sufficient — a compromised CDN or MITM-capable adversary can still serve valid HTTPS with bad content.
- **Content integrity + authenticity**: manifest is signed with **Ed25519**. Private key lives offline in the release/CI pipeline; public key is **embedded in the client binary at compile time**, never fetched at runtime (fetching it at runtime would let an attacker swap the key). The manifest lists a SHA-256 hash per platform binary; one signature covers the whole manifest.
- **Key rotation**: client trusts a small *set* of embedded public keys, not a single hardcoded one, from day one — even if only one key is populated initially. If a key is ever compromised, you need a way to invalidate it without permanently stranding already-deployed clients that only trust the old key.
- **Never execute-then-verify.** Download to a temp file, verify hash + signature completely, only then touch anything related to the live executable. Any verification failure → delete temp file, log, retry next cycle. Never fall back to "run it anyway."
- **Hash/signature is over the compressed artifact** (see §4) — the bytes that actually cross the wire — verified before decompression, not after.
- **OS code signing is a separate, complementary layer**: Apple notarization and Windows Authenticode are about the OS trusting the binary enough to execute; the Ed25519 layer is about the update client trusting that the bytes came from you. Both are needed — without OS signing, downloaded binaries get quarantined/flagged by the OS; without Ed25519, anyone who compromises your CDN or bucket owns every client.
- **Signature-mismatch reports get elevated handling** in telemetry (§5) — that's a tamper/MITM signal, not an ordinary network failure, and should alert distinctly.

## 2. Atomicity — a failed update must never leave a corrupt binary

- Download to a **temp file in the same directory** as the running executable (same volume is required — cross-volume "renames" silently become copies, which aren't atomic and can leave a half-written file in place if interrupted).
- Verify fully before any file at the target path is touched.
- **Linux / macOS**: `rename(2)` directly over the running executable's path. This is atomic and safe even while the old binary is executing — the running process holds the old inode open via file descriptor; rename just repoints the directory entry.
- **Windows**: can't overwrite a running executable's bytes, but can generally rename it (the loader typically opens it with `FILE_SHARE_DELETE`). Sequence: rename `app.exe` → `app.exe.old`, rename `app.new.exe` → `app.exe`, spawn the new process, exit the old one, delete `app.exe.old` on the next successful launch.
- **Single-instance lock** (`flock` / `LockFileEx`) ensures that if multiple copies of the app are running, only one performs the swap; the rest just detect the version bump and relaunch themselves naturally later.
- **Disk space preflight**: check available space before downloading. An out-of-space failure mid-write is exactly the "corrupt binary" scenario atomicity is meant to prevent — but only if it's caught before writing starts, not after.

## 3. Rollback — recover from a bad new version

- Keep the previous binary as `.old` for one generation after a swap.
- **Crash-loop detection**: write a small marker file before running "real" startup logic after an update, clear it once startup completes healthily. If the new version crashes within N seconds of relaunch (marker still present on next start), automatically swap back to `.old`.
- **Staged/canary rollout**: crash-loop detection catches a bad update on one client after the fact. It doesn't stop a bad release from hitting everyone at once. Add a `"rollout": <percentage>` field to the manifest; each client hashes its own install ID to deterministically decide whether it's in the cohort for a given release. Lets a broken build be caught at 10% before it reaches 100%.

## 4. Speed — large binary downloads

- **Compress at build time.** zstd over gzip — meaningfully better ratio and faster decompression, available via `klauspost/compress/zstd`. Strip debug symbols first (`-ldflags="-s -w"`) — most of what compresses well is repetitive metadata anyway. Go binaries typically shrink 40–60% with this combination.
- **Delta updates — deliberately out of scope** (see Assumptions). Go's linker layout shifts substantially with small source changes, so naive binary diffs often end up 30–50%+ of the full file anyway, for a lot of added serving complexity (storing/serving deltas between every version pair). Revisit only if there's real evidence users on slow/metered connections are failing updates because of size.
- **Resumable downloads**: HTTP `Range` requests so an interrupted download (sleep, wifi drop) resumes instead of restarting from zero. Exponential backoff with jitter on retry.
- **Streaming hash verification**: hash while downloading rather than buffering the whole file first, so verification isn't a second full pass over the data.
- **Jittered polling**: the hourly check itself uses a randomized offset (e.g. ±5 min) around the base interval, so many clients don't hammer the CDN at the top of every hour if this scales to a larger fleet.

## 5. Observability — know when and why updates fail in the field

- Since there's no custom application server, telemetry needs *somewhere* to land — even if that's just a minimal ingestion endpoint in front of a managed sink (Sentry, or a small serverless function writing to a log/DB). This is the one place the "no custom server" assumption gets stretched.
- **What to send**: OS/arch, old version → new version, outcome (success/fail), and an error *class* (network, hash mismatch, signature invalid, disk full, permission denied) — not raw error strings, which can leak local file paths or usernames.
- **Fire-and-forget, non-blocking, best-effort**, short timeout, single attempt. Telemetry failing must never affect the update path itself — same principle as making update-check errors non-fatal to the running app.
- Signature-mismatch events are reported and alerted on separately from ordinary network failures (see §1).

## 6. Other considerations

- **Install location & permissions**: if the binary lives in a system directory (`/usr/local/bin`, `C:\Program Files\`), the running process may lack write access to replace itself without elevation. A self-update that silently fails with "permission denied" on locked-down corporate machines is a real failure mode — default to a per-user install location where the process already owns the directory; only handle elevation if a system-wide install is specifically required.
- **Windows/macOS reputation lag**: a freshly signed Windows binary can still trip SmartScreen or AV heuristics for a while even with a valid Authenticode signature, until it accumulates reputation. Not something to engineer around — but worth knowing so a wave of "AV blocked the update" reports isn't mistaken for a real regression.
- **Update check failures must never be fatal to the app.** Offline laptop, corporate firewall, update server down — all of these should log and retry next cycle, never crash or degrade the running application.

---

## Manifest format

```json
{
  "version": "1.4.2",
  "rollout": 100,
  "platforms": {
    "darwin-arm64": {"url": "https://cdn.example.com/releases/1.4.2/app-darwin-arm64.zst", "sha256": "...", "size": 4210688},
    "darwin-amd64": {"...": "..."},
    "linux-amd64":  {"...": "..."},
    "windows-amd64":{"...": "..."}
  }
}
```
Signature travels alongside as `manifest.json.sig` (raw Ed25519 signature bytes, base64), covering the full manifest.

## Repo layout

```
/cmd/app                 — the actual deployed application
/cmd/releaser             — CI tool: builds, strips, compresses, hashes, signs manifest
/cmd/devserver            — trivial static file server for local testing
/internal/selfupdate
    poller.go             — hourly ticker with jitter, wraps the check
    check.go              — fetch + verify manifest, decide if newer version exists
    download.go           — resumable, streaming-hash download + decompression
    apply_unix.go         — rename-based swap (linux, darwin build tags)
    apply_windows.go      — rename-dance swap (windows build tag)
    lock.go               — cross-platform single-instance lock
    rollback.go           — crash-loop marker + revert to .old
    telemetry.go          — best-effort, non-blocking failure reporting
/internal/version         — embedded version string, semver compare
```

## Phased implementation plan

1. Core verify + manifest fetch — pure logic, platform-independent, most testable.
2. Unix atomic swap — simplest case, integration-tested against `httptest`.
3. Windows swap + relaunch — needs a Windows CI runner to trust it.
4. Compression + resumable/streaming download.
5. Rollback / crash-loop detection + staged rollout cohorting.
6. Telemetry reporting.
7. Releaser CLI + devserver — release pipeline glue.
