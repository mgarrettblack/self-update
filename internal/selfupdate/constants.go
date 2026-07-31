package selfupdate

import (
	"os"
	"time"
)

const (
	stateDirMode    os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
	lockDirMode     os.FileMode = 0o755
	lockFileMode    os.FileMode = 0o644
)

// ---------------------------------------------------------------------------
// OS layer — fs.go
// ---------------------------------------------------------------------------

// Disk-space preflight margin.
const spaceSafetyMargin = 32 << 20

// Retained old-binary suffix, for rollback.
const oldSuffix = ".old"

// Decompressed-size bound, capping a hostile or corrupt artifact.
const maxDecompressedBytes = 1 << 30

// Anonymous per-install identifier, for telemetry.
const (
	installIDFile  = "install-id"
	installIDBytes = 16
)

// Manifest/artifact fetch retry policy.
const (
	defaultFetchAttempts = 4
	defaultBaseBackoff   = time.Second
	maxBackoffDelay      = 30 * time.Second
	defaultFetchTimeout  = 15 * time.Minute
	downloadBufferSize   = 64 << 10
)

// Rollout gate default.
const defaultRolloutPercentage = 100

// Manifest and signature fetch limits.
const (
	defaultMaxManifestBytes = 1 << 20
	maxSignatureBytes       = 4 << 10
	defaultCheckTimeout     = 30 * time.Second

	// signatureURLSuffix is appended to the manifest URL. Deriving it rather
	// than configuring it separately rules out verifying release A's manifest
	// against release B's signature.
	signatureURLSuffix = ".sig"
)

// State-directory filenames.
const (
	// markerFilename is the crash-loop marker's name inside Guard.StateDir. It
	// lives there rather than next to the binary because the binary's
	// directory may be read-only for the user (§6), while state is always
	// per-user writable.
	markerFilename = "update-pending.json"

	// lockFilename is the single-instance updater lock, inside StateDir.
	lockFilename = "update.lock"
)

// Poll cadence.
const (
	// defaultPollInterval is hourly: the design's tradeoff between a security
	// fix reaching the fleet the same day and negligible load on the release
	// host.
	defaultPollInterval = time.Hour

	// pollJitterFraction spreads each client's schedule over [interval,
	// interval*1.5), so a fleet deployed together doesn't poll together
	// forever and turn every release into a self-inflicted thundering herd.
	pollJitterFraction = 0.5
)

// Staging-file suffixes.
const (
	// downloadSuffix and stagedSuffix name the two staging files. Both live in
	// the target's own directory: the swap is a rename, and a rename is only
	// atomic within a volume.
	downloadSuffix = ".download"
	stagedSuffix   = ".new"
)

// Decompression-preflight estimate.
//
// decompressionRatioEstimate over-estimates the artifact's expansion: the
// compressed size is the only figure the manifest carries, and running out of
// space mid-decompression is exactly what the preflight exists to prevent.
const decompressionRatioEstimate = 4

// ---------------------------------------------------------------------------
// Cross-cutting — telemetry.go
// ---------------------------------------------------------------------------

const (
	// defaultReportTimeout keeps a hung or black-holed ingestion endpoint from
	// pinning a goroutine for the lifetime of the process.
	defaultReportTimeout = 5 * time.Second

	// maxDrain is how much of a response body is read before closing, purely so
	// the keep-alive connection can be reused. The body itself is of no
	// interest.
	maxDrain = 4 << 10
)
