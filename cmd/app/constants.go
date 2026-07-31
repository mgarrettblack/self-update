package main

import "time"

const appName = "demoapp"
const defaultEnvPath = ".env.local"
const allowHTTPKey = "selfupdate_allow_http"

// Exit codes returned by run.
const (
	exitOK           = 0
	exitRuntimeError = 1
	exitUsageError   = 2
)

const (
	defaultPollInterval = time.Hour
	pollJitterFraction  = 0.5
)
