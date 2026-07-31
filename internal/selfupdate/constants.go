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

// Disk-space preflight margin.
const spaceSafetyMargin = 32 << 20

// Retained old-binary suffix, for rollback.
const oldSuffix = ".old"

// Decompressed-size bound, capping a hostile or corrupt artifact.
const maxDecompressedBytes = 1 << 30

// Anonymous per-install identifier, for rollout cohorting.
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
	signatureURLSuffix      = ".sig"
)

// State-directory filenames.
const (
	markerFilename = "update-pending.json"
	lockFilename   = "update.lock"
)

// Poll cadence.
const (
	defaultPollInterval = time.Hour
	pollJitterFraction  = 0.5
)

// Staging-file suffixes.
const (
	downloadSuffix = ".download"
	stagedSuffix   = ".new"
)

const decompressionRatioEstimate = 4
