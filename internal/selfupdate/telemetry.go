package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// Outcome is the result of an update attempt.
type Outcome string

const (
	OutcomeSuccess    Outcome = "success"
	OutcomeFailure    Outcome = "failure"
	OutcomeNoUpdate   Outcome = "no_update"
	OutcomeRolledBack Outcome = "rolled_back"
)

// Severity lets the sink alert differently on tamper signals than on the
// ordinary background noise of field failures.
//
// A hash or signature mismatch means the bytes on the wire did not match what
// the release pipeline signed — a compromised CDN or a MITM, not a flaky
// hotel network — so it is escalated to SeverityAlert while everything else
// stays at warn and gets looked at in aggregate.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityAlert Severity = "alert"
)

// Event is one update-attempt report. Every field is either a fixed
// enumeration or a version string: deliberately nothing free-form, because a
// raw error message can contain a local path or username.
type Event struct {
	InstallID   string     `json:"install_id"`
	OS          string     `json:"os"`
	Arch        string     `json:"arch"`
	FromVersion string     `json:"from_version"`
	ToVersion   string     `json:"to_version,omitempty"`
	Outcome     Outcome    `json:"outcome"`
	ErrorClass  ErrorClass `json:"error_class,omitempty"`
	Severity    Severity   `json:"severity"`
}

// Reporter sends events to an ingestion endpoint. A zero Endpoint disables
// reporting entirely, so telemetry is opt-in by configuration.
type Reporter struct {
	Endpoint  string
	InstallID string
	Client    *http.Client  // nil means a default client
	Timeout   time.Duration // 0 means a short default (a few seconds)
	UserAgent string

	// inFlight tracks goroutines spawned by Report so Wait can join them.
	inFlight sync.WaitGroup
}

// defaultReportClient is shared across Reporters that do not supply one, so
// repeated reports reuse connections. It has no Client.Timeout of its own; the
// per-request context carries the deadline.
var defaultReportClient = &http.Client{Transport: http.DefaultTransport}

// enabled reports whether this Reporter should send anything. A nil receiver
// and an unset Endpoint are both "disabled", which lets the poller hold an
// unconfigured Reporter and call it unconditionally.
func (r *Reporter) enabled() bool { return r != nil && r.Endpoint != "" }

// ReportFailure sends a failure event for err, deriving the error class with
// ClassOf and escalating severity for tamper signals.
func (r *Reporter) ReportFailure(fromVersion, toVersion string, err error) {
	if !r.enabled() {
		return
	}
	class := ClassOf(err)

	// Only a tamper signal — the bytes on the wire not matching what the
	// release pipeline signed — is escalated to SeverityAlert. Everything
	// else is ordinary field failure and stays at warn.
	severity := SeverityWarn
	if class.IsTamperSignal() {
		severity = SeverityAlert
	}

	r.Report(Event{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Outcome:     OutcomeFailure,
		ErrorClass:  class,
		Severity:    severity,
	})
}

// ReportSuccess sends a success event.
func (r *Reporter) ReportSuccess(fromVersion, toVersion string) {
	if !r.enabled() {
		return
	}
	r.Report(Event{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Outcome:     OutcomeSuccess,
		Severity:    SeverityInfo,
	})
}

// ReportRollback sends a rolled-back event.
//
// A rollback means crash-loop detection fired, so it is a warning rather than
// informational — but it is not a tamper signal: the binary that was installed
// is the one that was signed, it simply did not stay up.
func (r *Reporter) ReportRollback(fromVersion, toVersion string) {
	if !r.enabled() {
		return
	}
	r.Report(Event{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Outcome:     OutcomeRolledBack,
		Severity:    SeverityWarn,
	})
}

// Report sends ev fire-and-forget: it returns immediately and never blocks the
// caller, never retries, and never returns an error. Telemetry failing must not
// affect the update path.
func (r *Reporter) Report(ev Event) {
	if !r.enabled() {
		return
	}

	// Identity is stamped here rather than trusted from the caller so no call
	// site can omit it.
	ev.InstallID = r.InstallID
	ev.OS = runtime.GOOS
	ev.Arch = runtime.GOARCH
	if ev.Severity == "" {
		ev.Severity = SeverityInfo
	}

	r.inFlight.Add(1)
	go func() {
		defer r.inFlight.Done()
		// A pathological client or transport must not take the hosting
		// application down: recover and drop the event.
		defer func() { _ = recover() }()
		r.send(ev)
	}()
}

// send performs the single, deadline-bounded POST. Errors are deliberately
// swallowed: there is nothing useful to do with a failed telemetry write, and
// retrying would amplify load against an endpoint that is already unhappy.
func (r *Reporter) send(ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultReportTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}

	client := r.Client
	if client == nil {
		client = defaultReportClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	_ = resp.Body.Close()
}

// Wait blocks until in-flight reports finish. For tests and for a clean
// shutdown only — the update path never calls it.
func (r *Reporter) Wait() {
	if r == nil {
		return
	}
	r.inFlight.Wait()
}
