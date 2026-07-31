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

	// Step 1: check if previous update was successful
	if !selfupdate.UpdateSuccessful(cfg) {
		level.Error(logger).Log("msg", "previous update failed, rolling back")
		selfupdate.Rollback(cfg)
	}

	// Step 2: start the actual application
	if err := launchApp(logger); err != nil {
		level.Error(logger).Log("msg", "app startup failed", "err", err)
		return exitRuntimeError
	}
	cleanup(cfg, logger)

	// Step 3: poll for new updates
	return pollForUpdate(ctx, cfg, interval, logger)
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

func cleanup(cfg selfupdate.Config, logger log.Logger) {
	if err := os.Remove(cfg.TargetPath + selfupdate.DownloadSuffix); err != nil && !os.IsNotExist(err) {
		level.Warn(logger).Log("msg", "cleanup stale download artifact failed", "err", err)
	}
	if err := os.Remove(cfg.TargetPath + selfupdate.StagedSuffix); err != nil && !os.IsNotExist(err) {
		level.Warn(logger).Log("msg", "cleanup stale staged artifact failed", "err", err)
	}
}

func pollForUpdate(ctx context.Context, cfg selfupdate.Config,
	interval time.Duration, logger log.Logger) int {

	for {
		d, err := selfupdate.CheckForUpdate(ctx, cfg)
		switch {
		case err != nil:
			level.Warn(logger).Log("msg", "check failed", "err", err)
		case !d.UpdateAvailable:
			level.Debug(logger).Log("msg", "no update")
		default:
			if err := selfupdate.ApplyUpdate(ctx, cfg, d); err != nil {
				level.Error(logger).Log("msg", "apply failed", "err", err)
				break
			}
			level.Info(logger).Log("msg", "update applied", "from", d.CurrentVersion, "to", d.Manifest.Version)

			if err := selfupdate.Relaunch(cfg.TargetPath, os.Args); err != nil {
				level.Warn(logger).Log("msg", "relaunch failed, continuing", "err", err)
				break
			}
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

func nextInterval(base time.Duration) time.Duration {
	if base <= 0 {
		base = defaultPollInterval
	}
	return base + time.Duration(rand.Float64()*pollJitterFraction*float64(base))
}

func setup() (selfupdate.Config, time.Duration, error) {

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
