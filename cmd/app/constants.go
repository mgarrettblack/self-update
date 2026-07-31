package main

import "time"

// appName identifies this binary in logs and in the platform default state
// directory chosen by selfupdate.DefaultStateDir.
const appName = "demoapp"

// defaultEnvPath is the dotenv file setup reads, resolved relative to the
// working directory the demo is run from. There is no flag to override it.
const defaultEnvPath = ".env.local"

// Exit codes returned by run.
const (
	exitOK           = 0
	exitRuntimeError = 1
	exitUsageError   = 2
)

// Poll cadence. These live here rather than in the library because the library
// no longer sleeps: main owns the loop, so main owns its timing.
const (
	// defaultPollInterval is used when the dotenv sets no interval.
	defaultPollInterval = time.Hour

	// pollJitterFraction is the largest fraction of the base interval that
	// nextInterval may add, to spread checks across installs.
	pollJitterFraction = 0.5
)

// allowHTTPKey is the viper key for SELFUPDATE_ALLOW_HTTP, the library's
// plaintext-HTTP escape hatch. main only reads it to warn; the library is what
// acts on it.
const allowHTTPKey = "selfupdate_allow_http"
