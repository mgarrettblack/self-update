// Command app is a demonstration application that keeps itself up to date.
//
// It exists to show the intended shape of the integration, which is mostly
// about ordering:
//
//  1. Run the crash-loop check before anything else can crash.
//  2. Do the application's own startup.
//  3. Only then report healthy, which discards the rollback path.
//  4. Poll for updates in the background, for the life of the process.
//
// Build it with a version and a trust set:
//
//	go build -ldflags "\
//	  -X self-update/internal/selfupdate.Version=1.4.2 \
//	  -X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY" ./cmd/app
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/spf13/viper"

	"self-update/internal/selfupdate"
)

const appName = "demoapp"

// defaultEnvPath is the default value of the -env flag.
const defaultEnvPath = ".env.local"

// Exit codes returned by run.
const (
	exitOK           = 0
	exitRuntimeError = 1
	exitUsageError   = 2
)

func main() {
	// A signal-cancelled context rather than a signal handler that exits: the
	// poller may be mid-download, and it needs the chance to clean up its
	// staging files.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// config is loaded from the env file named by -env.
type config struct {
	ManifestURL string        `mapstructure:"manifest_url"`
	Target      string        `mapstructure:"target"`
	StateDir    string        `mapstructure:"state_dir"`
	Interval    time.Duration `mapstructure:"interval"`
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	s := setup(args, stdout, stderr)
	if s.exit {
		return s.code
	}
	poller, logger := s.poller, s.logger

	// Step 1, before any work that could plausibly crash: reverts and
	// relaunches if the previous post-update start never got healthy.
	if !UpdateSuccessful(poller, logger) {
		level.Info(logger).Log("msg", "failed to update successful update", "path", poller)
		selfupdate.Rollback()
	}

	// Step 2: the application's real startup.
	LaunchApp(logger)

	// Step 3, not before: MarkHealthy discards the retained previous binary,
	// so crash-loop protection depends on it running after startup can fail.
	if err := poller.HealthCheck(); err != nil {
		level.Error(logger).Log("msg", "marking this build healthy", "err", err)
	}

	// Step 4: poll for the life of the process.
	switch err := poller.Poll(ctx); {
	case errors.Is(err, selfupdate.ErrRestartRequired):
		level.Info(logger).Log("msg", "shutting down so the updated binary can take over")
		return exitOK
	case err != nil:
		level.Error(logger).Log("msg", "poller run failed", "err", err)
		return exitRuntimeError
	}
	return exitOK
}

func UpdateSuccessful(poller *selfupdate.Poller, logger log.Logger) bool {
	// Startup checks if the previous update failed. If so, it internally:
	// 1. Restores the .old binary (RestoreOld)
	// 2. Clears the crash-loop marker
	// 3. Relaunches into the restored binary (which exits this process via exec)
	_, err := poller.Startup()
	if err != nil {
		level.Error(logger).Log("msg", "rollback check", "err", err)
		return false
	}
	return true
}

func LaunchApp(logger log.Logger) {
	level.Info(logger).Log("msg",
		"starting", "app",
		appName, "version",
		selfupdate.Version,
		"os",
		selfupdate.PlatformKey())
}

type setupResult struct {
	poller *selfupdate.Poller
	logger log.Logger
	code   int
	exit   bool
}

// setup parses flags, loads the config file and constructs the poller.
func setup(args []string, stdout, stderr io.Writer) setupResult {
	logger := newLogger(stdout)

	envPath, earlyExit := resolveFlags(args, stdout, stderr, logger)
	if earlyExit != nil {
		return *earlyExit
	}

	cfg, err := loadConfig(envPath)
	if err != nil {
		level.Error(logger).Log("msg", "loading config", "path", envPath, "err", err)
		return setupResult{logger: logger, code: exitUsageError, exit: true}
	}

	poller, err := newPoller(cfg, logger)
	if err != nil {
		level.Error(logger).Log("msg", "constructing poller", "err", err)
		return setupResult{logger: logger, code: exitRuntimeError, exit: true}
	}
	return setupResult{poller: poller, logger: logger}
}

// newLogger builds the leveled, timestamped logger used for the life of the
// process, including during flag parsing and config loading.
func newLogger(stdout io.Writer) log.Logger {
	logger := level.NewFilter(log.NewLogfmtLogger(stdout), level.AllowAll())
	return log.With(logger, "ts", log.DefaultTimestampUTC)
}

func resolveFlags(args []string, stdout, stderr io.Writer, logger log.Logger) (envPath string, earlyExit *setupResult) {
	envPath, printVersion, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", &setupResult{logger: logger, code: exitOK, exit: true}
		}
		level.Error(logger).Log("msg", "parsing flags", "err", err)
		return "", &setupResult{logger: logger, code: exitUsageError, exit: true}
	}
	if printVersion {
		fmt.Fprintf(stdout, "%s %s (%s)\n", appName, selfupdate.Version, selfupdate.PlatformKey())
		return "", &setupResult{logger: logger, code: exitOK, exit: true}
	}
	return envPath, nil
}

// infoLogf adapts a leveled logger to the Poller.Logf callback shape.
func infoLogf(logger log.Logger) func(string, ...any) {
	return func(format string, a ...any) {
		level.Info(logger).Log("msg", fmt.Sprintf(format, a...))
	}
}

func parseFlags(args []string, stderr io.Writer) (envPath string, printVersion bool, err error) {
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&envPath, "env", defaultEnvPath, "path to the env file")
	fs.BoolVar(&printVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return "", false, err
	}
	return envPath, printVersion, nil
}

func loadConfig(path string) (config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("env")
	if err := v.ReadInConfig(); err != nil {
		return config{}, err
	}
	var cfg config
	if err := v.Unmarshal(&cfg); err != nil {
		return config{}, err
	}
	if cfg.ManifestURL == "" {
		return config{}, fmt.Errorf("%s: manifest_url is required", path)
	}
	return cfg, nil
}

func newPoller(cfg config, logger log.Logger) (*selfupdate.Poller, error) {
	logf := infoLogf(logger)

	// First, because everything else is pointless without it.
	verifier, err := selfupdate.TrustedVerifier()
	if err != nil {
		return nil, err
	}

	stateDir := cfg.StateDir
	if stateDir == "" {
		if stateDir, err = selfupdate.DefaultStateDir(appName); err != nil {
			return nil, err
		}
	}
	installID, err := selfupdate.InstallID(stateDir)
	if err != nil {
		return nil, err
	}

	p := &selfupdate.Poller{
		Checker: &selfupdate.Checker{
			ManifestURL: cfg.ManifestURL,
			Verifier:    verifier,
			InstallID:   installID,
			UserAgent:   appName + "/" + selfupdate.Version,
		},
		Downloader: &selfupdate.Downloader{
			Progress: func(downloaded, total int64) {
				if total > 0 {
					level.Info(logger).Log("msg", "downloading", "percent", downloaded*100/total)
				}
			},
		},
		TargetPath: cfg.Target,
		StateDir:   stateDir,
		Interval:   cfg.Interval,
		Logf:       logf,
		Logger:     logger,
	}
	return p, nil
}
