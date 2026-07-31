// Command app is a demonstration application that keeps itself up to date.
//
// It exists to document the one call ordering the library cannot enforce for
// itself: startup check, then the real startup work, then MarkHealthy, and only
// then the poll loop. The four steps in run are numbered for that reason —
// moving MarkHealthy ahead of the work that can fail defeats rollback entirely.
package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/spf13/viper"

	"self-update/internal/selfupdate"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Stdout))
}

// config mirrors the dotenv at defaultEnvPath.
type config struct {
	ManifestURL string        `mapstructure:"manifest_url"`
	Target      string        `mapstructure:"target"`
	StateDir    string        `mapstructure:"state_dir"`
	Interval    time.Duration `mapstructure:"interval"`
}

func run(ctx context.Context, stdout io.Writer) int {
	logger := newLogger(stdout)

	cfg, interval, err := setup(logger)
	if err != nil {
		level.Error(logger).Log("msg", "setup", "err", err)
		return exitUsageError
	}

	// Step 1: a marker that survived means the last update never reported
	// healthy. Before any work that could itself crash.
	res, err := selfupdate.CheckStartup(cfg)
	if err != nil {
		level.Error(logger).Log("msg", "startup check", "err", err)
	}
	if res.Reverted {
		Rollback(cfg, res, err, logger)
	}

	// Step 2: the real startup work — everything that can fail.
	if err := launchApp(logger); err != nil {
		level.Error(logger).Log("msg", "app startup failed", "err", err)
		return exitRuntimeError
	}

	// Step 3: and not before. Clears the marker, drops the retained .old.
	if err := selfupdate.MarkHealthy(cfg); err != nil {
		level.Error(logger).Log("msg", "mark healthy", "err", err)
	}
	if err := selfupdate.RemoveOld(cfg.TargetPath); err != nil {
		level.Error(logger).Log("msg", "remove old binary", "err", err)
	}

	// Step 4: poll.
	return pollForUpdate(ctx, cfg, interval, logger)
}

func Rollback(cfg selfupdate.Config, res selfupdate.StartupResult, checkErr error, logger log.Logger) {
	// Marker is nil on the unparseable-marker path: there was no attempt
	// count to trust, so the revert fired without a version pair to report.
	from, to := "", ""
	if res.Marker != nil {
		from, to = res.Marker.FromVersion, res.Marker.ToVersion
	}
	level.Warn(logger).Log("msg", "update rolled back", "from", from, "to", to)

	if checkErr != nil {
		level.Error(logger).Log("msg", "revert incomplete, not relaunching",
			"target", cfg.TargetPath)
	} else if err := selfupdate.Relaunch(cfg.TargetPath, os.Args); err != nil {
		level.Error(logger).Log("msg", "relaunch after rollback", "err", err)
	}
}

// launchApp stands in for the application's real startup: whatever must succeed
// before this generation of the binary can be called healthy.
func launchApp(logger log.Logger) error {
	level.Info(logger).Log("msg", "starting",
		"app", appName,
		"version", selfupdate.Version,
		"os", selfupdate.PlatformKey())
	return nil
}

func pollForUpdate(ctx context.Context, cfg selfupdate.Config,
	interval time.Duration, logger log.Logger) int {

	for {
		d, err := selfupdate.CheckForUpdate(ctx, cfg)
		switch {
		case err != nil:
			// Never fatal: an unreachable release host must not take down the
			// application it exists to maintain.
			level.Warn(logger).Log("msg", "check failed",
				"class", selfupdate.ClassOf(err), "err", err)
		case !d.UpdateAvailable:
			level.Debug(logger).Log("msg", "no update", "reason", d.Reason)
		default:
			if err := selfupdate.ApplyUpdate(ctx, cfg, d); err != nil {
				level.Error(logger).Log("msg", "apply failed",
					"class", selfupdate.ClassOf(err), "err", err)
				break
			}
			level.Info(logger).Log("msg", "update applied",
				"from", d.CurrentVersion, "to", d.Manifest.Version)

			if err := selfupdate.Relaunch(cfg.TargetPath, os.Args); err != nil {
				// The swap succeeded and the marker is in place; we just could
				// not hand over. Staying on the old image beats exiting, and
				// the next start picks up the new binary.
				level.Warn(logger).Log("msg", "relaunch failed, continuing", "err", err)
				break
			}
			// Unix never reaches here — Relaunch replaced the image. On Windows
			// the successor is running and this process must exit.
			level.Info(logger).Log("msg", "exiting for successor")
			return exitOK
		}

		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(nextInterval(interval)):
		}
	}
}

// nextInterval returns the base interval plus jitter, to spread load across
// installs. math/rand is correct here: nothing about it is a security decision.
func nextInterval(base time.Duration) time.Duration {
	if base <= 0 {
		base = defaultPollInterval
	}
	return base + time.Duration(rand.Float64()*pollJitterFraction*float64(base))
}

// setup loads the dotenv and resolves it into the library's Config, returning
// the base poll interval alongside it.
func setup(logger log.Logger) (selfupdate.Config, time.Duration, error) {
	warnIfHTTPAllowed(logger)

	c, err := loadConfig(defaultEnvPath)
	if err != nil {
		return selfupdate.Config{}, 0, err
	}

	stateDir := c.StateDir
	if stateDir == "" {
		if stateDir, err = selfupdate.DefaultStateDir(appName); err != nil {
			return selfupdate.Config{}, 0, err
		}
	}

	cfg, err := selfupdate.NewConfig(c.ManifestURL, c.Target, stateDir)
	if err != nil {
		return selfupdate.Config{}, 0, err
	}
	return cfg, c.Interval, nil
}

func newLogger(stdout io.Writer) log.Logger {
	logger := level.NewFilter(log.NewLogfmtLogger(stdout), level.AllowAll())
	return log.With(logger, "ts", log.DefaultTimestampUTC)
}

// warnIfHTTPAllowed makes the library's SELFUPDATE_ALLOW_HTTP escape hatch
// visible in the log. It reads through a viper instance of its own so that
// binding the environment cannot change how the dotenv keys resolve.
func warnIfHTTPAllowed(logger log.Logger) {
	env := viper.New()
	env.AutomaticEnv()
	if env.GetBool(allowHTTPKey) {
		level.Warn(logger).Log("msg",
			"plaintext HTTP permitted for manifest and artifact fetches; "+
				"an attacker who can rewrite responses controls what this process runs next",
			"env", "SELFUPDATE_ALLOW_HTTP")
	}
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
