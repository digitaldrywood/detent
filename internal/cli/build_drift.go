package cli

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
)

const (
	defaultBuildDriftInterval = time.Minute
	buildDriftReadTimeout     = 5 * time.Second
)

type installedBuildReader func(context.Context) (buildinfo.Info, string, error)

func runRuntimeBuildDriftMonitor(
	ctx context.Context,
	runningBuild buildinfo.Info,
	interval time.Duration,
	read installedBuildReader,
	logger *slog.Logger,
) {
	if buildinfo.IsZero(runningBuild) || read == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = defaultBuildDriftInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	driftDetected := false
	lastReadError := ""
	for waitForBuildDriftCheck(ctx, interval) {
		readCtx, cancel := context.WithTimeout(ctx, buildDriftReadTimeout)
		installedBuild, path, err := read(readCtx)
		cancel()
		if err != nil {
			if message := err.Error(); message != lastReadError && ctx.Err() == nil {
				logger.Warn("check installed Detent build drift failed", "binary", path, "error", err)
				lastReadError = message
			}
			continue
		}
		lastReadError = ""
		drift := buildinfo.DetectDrift(runningBuild, installedBuild)
		if !drift.Comparable {
			continue
		}
		if drift.Detected {
			if !driftDetected {
				logger.Warn(
					"running Detent build differs from installed binary; restart required",
					"binary", path,
					"running_version", runningBuild.Version,
					"running_commit", buildinfo.ShortCommit(runningBuild.Commit),
					"installed_version", installedBuild.Version,
					"installed_commit", buildinfo.ShortCommit(installedBuild.Commit),
					"remediation", "detent start --restart",
				)
			}
			driftDetected = true
			continue
		}
		if driftDetected {
			logger.Info(
				"running Detent build now matches installed binary",
				"binary", path,
				"version", runningBuild.Version,
				"commit", buildinfo.ShortCommit(runningBuild.Commit),
			)
		}
		driftDetected = false
	}
}

func newInstalledExecutableBuildReader(executable func() (string, error), run buildinfo.BinaryRunner) installedBuildReader {
	path, pathErr := executable()
	path = strings.TrimSuffix(strings.TrimSpace(path), " (deleted)")
	return func(ctx context.Context) (buildinfo.Info, string, error) {
		if pathErr != nil {
			return buildinfo.Info{}, path, pathErr
		}
		info, err := buildinfo.ReadBinary(ctx, path, run)
		return info, path, err
	}
}

func waitForBuildDriftCheck(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
