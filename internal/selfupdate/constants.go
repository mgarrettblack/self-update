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

// Crash-loop budget: how many starts a pending update gets to report healthy
// before CheckStartup reverts it. One means the very next start decides — a
// build that cannot get through startup once will not get through it twice.
const maxStartAttempts = 1

// Staging-file suffixes. Exported so callers doing their own crash-recovery
// cleanup (e.g. a startup sweep before ApplyUpdate ever runs) can name the
// same files ApplyUpdate stages, without a dedicated library function.
const (
	DownloadSuffix = ".download"
	StagedSuffix   = ".new"
)
