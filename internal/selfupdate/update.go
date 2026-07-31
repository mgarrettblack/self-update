package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

type Checker struct {
	ManifestURL    string
	Verifier       *Verifier
	CurrentVersion string

	InstallID string

	Platform string

	Client    *http.Client
	UserAgent string

	MaxManifestBytes int64

	AllowInsecureManifestURL bool

	AllowInsecureArtifactURL bool
}

// Decision is the outcome of a check. Manifest is populated whenever a manifest
// was fetched and verified, even when no update applies, so the caller can log
// what the published version is.
type Decision struct {
	Manifest        *Manifest
	Artifact        PlatformArtifact
	UpdateAvailable bool

	// Reason explains the outcome in one line, for logs. It is always set.
	Reason string

	CurrentVersion string
}

// GB change func name to CheckForUpdate
func (c *Checker) Check(ctx context.Context) (*Decision, error) {
	const op = "check for update"

	// Fail closed before touching the network: a client with no trust set must
	// not fetch a manifest it has no way to verify, because the only thing it
	// could do with the result is act on unverified data.
	if c.Verifier == nil {
		return nil, classify(ClassInternal, op, errors.New(
			"no verifier configured; refusing to check for updates that cannot be verified"))
	}
	if strings.TrimSpace(c.ManifestURL) == "" {
		return nil, classify(ClassInternal, op, errors.New("manifest URL is empty"))
	}
	if err := requireHTTPS(c.ManifestURL, c.AllowInsecureManifestURL, "manifest", ClassInternal); err != nil {
		return nil, err
	}

	httpClient := c.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultCheckTimeout}
	}
	maxManifestBytes := c.MaxManifestBytes
	if maxManifestBytes <= 0 {
		maxManifestBytes = defaultMaxManifestBytes
	}

	current := c.CurrentVersion
	if current == "" {
		current = Version
	}
	if _, err := parseSemver(current); err != nil {
		return nil, classifyf(ClassInternal, op, "running version %q is not valid semver: %v", current, err)
	}

	body, err := fetchBytes(ctx, httpClient, c.ManifestURL, c.UserAgent, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	rawSig, err := fetchBytes(ctx, httpClient, c.ManifestURL+signatureURLSuffix, c.UserAgent, maxSignatureBytes)
	if err != nil {
		return nil, err
	}
	sig, err := DecodeSignature(rawSig)
	if err != nil {
		return nil, err
	}
	if err := c.Verifier.Verify(body, sig); err != nil {
		return nil, err
	}

	// Trusted from here on, and only from here on.
	m, err := ParseManifest(body)
	if err != nil {
		return nil, err
	}

	d := &Decision{Manifest: m, CurrentVersion: current}

	newer, err := IsNewer(m.Version, current)
	if err != nil {
		return nil, classify(ClassManifestInvalid, op, err)
	}
	if !newer {
		// Covers both "same version" and a rolled-back manifest advertising an
		// older release; neither should move this client.
		d.Reason = fmt.Sprintf("running %s, published release is %s", current, m.Version)
		return d, nil
	}

	platform := c.Platform
	if platform == "" {
		platform = PlatformKey()
	}
	art, err := m.Artifact(platform)
	if err != nil {
		d.Reason = fmt.Sprintf("release %s publishes no artifact for %s", m.Version, platform)
		return d, nil
	}
	if err := requireHTTPS(art.URL, c.AllowInsecureArtifactURL, "artifact", ClassManifestInvalid); err != nil {
		return nil, err
	}

	if !InRolloutCohort(c.InstallID, m.Version, m.RolloutPercent()) {
		d.Reason = fmt.Sprintf("release %s is at %d%% rollout and this install is not in the cohort",
			m.Version, m.RolloutPercent())
		return d, nil
	}

	d.Artifact = art
	d.UpdateAvailable = true
	d.Reason = fmt.Sprintf("update available: %s to %s", current, m.Version)
	return d, nil
}

type Marker struct {
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	AppliedAt   time.Time `json:"applied_at"`
	Attempts    int       `json:"attempts"`
}

type Guard struct {
	StateDir    string                    // marker lives here
	BinaryPath  string                    // binary to revert
	MaxAttempts int                       // 0 means default 1
	Now         func() time.Time          // nil means time.Now
	Restore     func(target string) error // nil means RestoreOld
}

// StartupResult is what CheckStartup observed.
type StartupResult struct {
	Reverted bool
	Marker   *Marker // the marker that was found, if any
}

func markerPath(g *Guard) string { return filepath.Join(g.StateDir, markerFilename) }

func MarkPending(g *Guard, fromVersion, toVersion string) error {
	if g.StateDir == "" {
		return classifyf(ClassInternal, "mark pending", "no state dir configured")
	}
	if err := os.MkdirAll(g.StateDir, stateDirMode); err != nil {
		return classify(ClassOf(err), "mark pending: create state dir", err)
	}

	now := g.Now
	if now == nil {
		now = time.Now
	}
	m := Marker{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		AppliedAt:   now(),
		Attempts:    0,
	}
	return writeMarker(g, m)
}

// CheckStartup runs once at process start, before any real startup work, and
// reverts to the previous binary if a marker survived from an update that
// never reached MarkHealthy. See rollback.md for the attempt-accounting
// walkthrough and what StartupResult.Reverted means for the caller.
func CheckStartup(g *Guard) (StartupResult, error) {
	if g.StateDir == "" {
		return StartupResult{}, classifyf(ClassInternal, "check startup", "no state dir configured")
	}

	data, err := os.ReadFile(markerPath(g))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StartupResult{}, nil
		}
		return StartupResult{}, classify(ClassOf(err), "check startup: read marker", err)
	}

	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {

		return Rollback(g, nil)
	}

	m.Attempts++

	maxAttempts := g.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if m.Attempts > maxAttempts {
		return Rollback(g, &m)
	}

	if err := writeMarker(g, m); err != nil {
		// The attempt could not be recorded. Proceed rather than fail: an
		// unwritable state dir must not stop the app from starting.
		return StartupResult{Marker: &m}, err
	}
	return StartupResult{Marker: &m}, nil
}

// MarkHealthy clears the marker once startup has completed successfully. It is
// a no-op when there is no marker, so it is safe to call unconditionally at the
// end of startup on every run, not just post-update ones.
func MarkHealthy(g *Guard) error {
	if g.StateDir == "" {
		return classifyf(ClassInternal, "mark healthy", "no state dir configured")
	}
	if err := clearMarker(g); err != nil {
		return classify(ClassOf(err), "mark healthy", err)
	}
	return nil
}

// Rollback puts the previous generation back and clears the marker. found is
// the marker that triggered it, or nil if it was unparseable.
// GB Rollback should not return anything
func Rollback(g *Guard, found *Marker) {
	// Step 1: rename <BinaryPath>.old back onto BinaryPath. g.Restore exists
	restore := g.Restore
	if restore == nil {
		restore = RestoreOld
	}
	err := restore(g.BinaryPath)
	if err != nil {
		// log error
	}

	// Step 2: remove the marker file, unconditionally — even if step 1 failed.
	// GB change func to RemoveFile, pass path args, return an error func, if error, just log an error
	clearErr := clearMarker(g)
	if err != nil {
		// log error
	}

}

// writeMarker persists m via a temp file plus rename, so a crash mid-write can
// never leave a partial marker at markerPath.
func writeMarker(g *Guard, m Marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return classify(ClassInternal, "write marker", err)
	}

	tmp := markerPath(g) + ".tmp"
	if err := os.WriteFile(tmp, data, privateFileMode); err != nil {
		return classify(ClassOf(err), "write marker", err)
	}
	if err := os.Rename(tmp, markerPath(g)); err != nil {
		_ = os.Remove(tmp)
		return classify(ClassOf(err), "write marker: rename", err)
	}
	return nil
}

func clearMarker(g *Guard) error {
	if err := os.Remove(markerPath(g)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ErrRestartRequired reports that an update was applied and a successor process
// has been started, so this process must shut down and exit.
//
// On unix this is never returned from a successful update: Relaunch replaces
// the process image and does not come back. It is the Windows path, where the
// successor is a new process and the outgoing one has to exit before its
// executable can be cleaned up.
var ErrRestartRequired = errors.New("update applied: this process must exit so its successor can take over")

// Poller owns the update lifecycle: it checks on a jittered schedule, and for
// each accepted release runs preflight, download, verify, decompress, swap,
// mark, relaunch in that order.
//
// Nothing here is best-effort. Every step that could leave the installation
// half-updated is ordered so that a failure at any point leaves the live binary
// exactly as it was.
type Poller struct {
	// Checker is required.
	Checker *Checker

	// Nil means a default Downloader.
	Downloader *Downloader

	// Logger: nil discards log lines. Uses the info/warn/error split that
	// Reporter's Severity used to carry.
	Logger log.Logger

	// TargetPath: empty means the running executable, with symlinks resolved.
	TargetPath string

	// StateDir holds the lock, the install id and the crash-loop marker.
	// Required — it must be writable even when the install directory is not.
	StateDir string

	// LockPath: empty means StateDir/update.lock.
	LockPath string

	// Interval: zero means one hour.
	Interval time.Duration

	// MaxStartAttempts: zero means one.
	MaxStartAttempts int

	// Argv: empty means os.Args.
	Argv []string

	// RequireConfirmation is consulted after a release is verified as
	// applicable and before anything is downloaded. Returning false declines the
	// update, which is a normal outcome and not an error. Nil means updates are
	// applied without asking.
	RequireConfirmation func(*Decision) bool

	// Relaunch: for tests. Nil means Relaunch.
	Relaunch func(path string, argv []string) error

	// Logf: nil discards log lines.
	Logf func(format string, args ...any)

	// ReportNoUpdate logs a line on every "already current" check, not just a
	// change. Off by default: on an hourly poll that's an hourly log line for
	// no news, which is noise more than signal, so it is opt-in.
	ReportNoUpdate bool
}

// UpdateResult describes one completed cycle.
type UpdateResult struct {
	// Decision is the check outcome. Nil only when the check itself failed.
	Decision *Decision

	Applied bool

	// RestartPending is set when a successor process was started and this
	// process must exit. See ErrRestartRequired.
	RestartPending bool
}

// Poll checks immediately and then on a jittered schedule until ctx is done.
//
// A failed check is logged and retried on the next tick, never returned: the
// updater is a background concern, and an unreachable release host must not take
// down the application it is supposed to be maintaining. Poll returns nil when
// ctx is done, or ErrRestartRequired when the caller must exit.
// GB change func name to Poll
func (p *Poller) Poll(ctx context.Context) error {
	for {
		res, err := p.Update(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			p.logf("update check failed (%s): %v", ClassOf(err), err)
		case res.RestartPending:
			return ErrRestartRequired
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.nextInterval()):
		}
	}
}

// Update runs a single cycle.
func (p *Poller) Update(ctx context.Context) (UpdateResult, error) {
	var res UpdateResult

	if p.Checker == nil {
		return res, classifyf(ClassInternal, "update", "no checker configured")
	}
	if p.StateDir == "" {
		return res, classifyf(ClassInternal, "update", "no state directory configured")
	}
	target, err := p.target()
	if err != nil {
		return res, err
	}

	// Step 1: check for update — fetch, verify and parse the manifest, then
	// decide whether a newer release applies to this install.
	d, err := p.Checker.Check(ctx)
	if err != nil {
		logFailure(p.logger(), p.Checker.CurrentVersion, "", err)
		return res, err
	}
	res.Decision = d

	// Step 2: no update available — nothing further to do this cycle.
	if !d.UpdateAvailable {
		p.logf("no update: %s", d.Reason)
		if p.ReportNoUpdate {
			level.Info(p.logger()).Log("msg", "no update", "version", d.CurrentVersion, "reason", d.Reason)
		}
		return res, nil
	}

	// Step 3: confirmation gate — optional, consulted before anything is
	// downloaded.
	if p.RequireConfirmation != nil && !p.RequireConfirmation(d) {
		// Asked before downloading, not after: consent that arrives once the
		// bytes are already on disk is not much of a choice.
		p.logf("update to %s declined", d.Manifest.Version)
		return res, nil
	}

	// Step 4: apply the update — download, decompress, swap and mark pending.
	if err := p.apply(ctx, target, d); err != nil {
		logFailure(p.logger(), d.CurrentVersion, d.Manifest.Version, err)
		return res, err
	}
	res.Applied = true

	level.Info(p.logger()).Log("msg", "update applied", "from", d.CurrentVersion, "to", d.Manifest.Version)
	p.logf("updated %s to %s", d.CurrentVersion, d.Manifest.Version)

	// Step 5: relaunch into the new binary.
	if err := p.relaunch(target); err != nil {
		// The swap succeeded, so the update is real and the marker is in place;
		// we simply could not hand over. Staying alive on the old image is
		// strictly better than exiting, and the next start picks up the new one.
		p.logf("relaunch failed, continuing on the current process: %v", err)
	} else {
		res.RestartPending = true
	}
	return res, nil
}

// apply performs the part of the cycle that touches the filesystem. It is
// separated out so every failure path runs the same cleanup.
func (p *Poller) apply(ctx context.Context, target string, d *Decision) error {
	dir := filepath.Dir(target)
	compressed := target + downloadSuffix
	staged := target + stagedSuffix

	// The lock covers download through swap. Two instances staging to the same
	// paths would interleave writes into one file and produce a binary that
	// matches no digest at all.
	if err := os.MkdirAll(p.StateDir, stateDirMode); err != nil {
		return classify(ClassOf(err), "create state directory", err)
	}
	lockPath := p.LockPath
	if lockPath == "" {
		lockPath = filepath.Join(p.StateDir, lockFilename)
	}
	// Step 4a: acquire the update lock — covers download through swap.
	lock, err := AcquireLock(lockPath)
	if err != nil {
		return err
	}

	defer func() {
		os.Remove(compressed)
		os.Remove(staged)
		lock.Release()
	}()

	// Step 4b: preflight disk space — ensureFreeSpace checks the artifact and
	// its decompressed expansion, on the volume that will hold both. Checked
	// before the first byte is requested.
	need := d.Artifact.Size * (1 + decompressionRatioEstimate)
	if need < d.Artifact.Size { // overflow on an absurd declared size
		return classifyf(ClassManifestInvalid, "update",
			"declared artifact size %d bytes is not plausible", d.Artifact.Size)
	}
	if err := ensureFreeSpace(dir, need); err != nil {
		return err
	}

	// Downloads, decompresses and swaps the artifact onto target. Each step
	// only runs once the previous one's bytes are verified.
	downloader := p.Downloader
	if downloader == nil {
		downloader = &Downloader{}
	}
	// Step 4c: download — Fetch verifies SHA-256 over the compressed bytes,
	// the ones the signed manifest covers, and returns nil only if they match.
	if err := downloader.Fetch(ctx, d.Artifact, compressed); err != nil {
		return err
	}
	// Step 4d: decompress — only now, on bytes that are already verified.
	if err := DecompressFile(compressed, staged); err != nil {
		return err
	}
	// Step 4e: apply — swap the staged binary onto the target.
	if err := Apply(staged, target); err != nil {
		return err
	}

	// Step 4f: mark pending — recorded after the swap, before relaunch.
	return MarkPending(p.guard(target), d.CurrentVersion, d.Manifest.Version)
}

func (p *Poller) Startup() (StartupResult, error) {
	target, err := p.target()
	if err != nil {
		return StartupResult{}, err
	}

	res, err := CheckStartup(p.guard(target))
	if err != nil {
		return res, err
	}
	if !res.Reverted {
		return res, nil
	}

	from, to := "", ""
	if res.Marker != nil {
		from, to = res.Marker.FromVersion, res.Marker.ToVersion
	}
	p.logf("update to %s never reported healthy; reverted to %s", to, from)
	level.Warn(p.logger()).Log("msg", "update rolled back", "from", from, "to", to)

	if err := p.relaunch(target); err != nil {
		p.logf("could not relaunch the restored binary: %v", err)
	}
	return res, nil
}

// HealthCheck records that this build started successfully, which cancels the
// pending revert and discards the retained previous generation.
func (p *Poller) HealthCheck() error {
	target, err := p.target()
	if err != nil {
		return err
	}
	if err := MarkHealthy(p.guard(target)); err != nil {
		return err
	}

	return RemoveOld(target)
}

// target resolves the executable to replace, following symlinks.
func (p *Poller) target() (string, error) {
	const op = "resolve target binary"

	path := p.TargetPath
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", classify(ClassInternal, op, err)
		}
		path = exe
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", classify(ClassInternal, op, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// A target that does not exist yet is a first install, not an error.
	return abs, nil
}

func (p *Poller) guard(target string) *Guard {
	return &Guard{
		StateDir:    p.StateDir,
		BinaryPath:  target,
		MaxAttempts: p.MaxStartAttempts,
	}
}

func (p *Poller) relaunch(target string) error {
	argv := p.Argv
	if len(argv) == 0 {
		argv = os.Args
	}
	if p.Relaunch != nil {
		return p.Relaunch(target, argv)
	}
	return Relaunch(target, argv)
}

// nextInterval returns the base interval plus jitter.
func (p *Poller) nextInterval() time.Duration {
	base := p.Interval
	if base <= 0 {
		base = defaultPollInterval
	}
	// rand is fine here and a CSPRNG would be misleading: this only spreads
	// load, and nothing about it is a security decision.
	return base + time.Duration(rand.Float64()*pollJitterFraction*float64(base))
}

func (p *Poller) logf(format string, args ...any) {
	if p.Logf == nil {
		return
	}
	p.Logf(format, args...)
}

func (p *Poller) logger() log.Logger {
	if p.Logger == nil {
		return log.NewNopLogger()
	}
	return p.Logger
}

func logFailure(logger log.Logger, from, to string, err error) {
	class := ClassOf(err)
	lvl := level.Warn
	if class.IsTamperSignal() {
		lvl = level.Error
	}
	lvl(logger).Log("msg", "update failed", "from", from, "to", to, "class", class, "err", err)
}
