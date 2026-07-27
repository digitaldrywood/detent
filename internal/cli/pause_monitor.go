package cli

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/pause"
)

const defaultPauseExitPollInterval = 30 * time.Second

type pauseMonitorDeps struct {
	read          func() (globalconfig.Config, error)
	write         func(globalconfig.Config) error
	connectorFor  func(string) connector.Connector
	repositoryFor func(string) string
	now           func() time.Time
	interval      time.Duration
	logger        *slog.Logger
}

func runPauseMonitor(ctx context.Context, deps pauseMonitorDeps) {
	deps = normalizePauseMonitorDeps(deps)
	checkPauseExitConditions(ctx, deps)

	ticker := time.NewTicker(deps.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkPauseExitConditions(ctx, deps)
		}
	}
}

func checkPauseExitConditions(ctx context.Context, deps pauseMonitorDeps) {
	deps = normalizePauseMonitorDeps(deps)
	cfg, err := deps.read()
	if err != nil {
		deps.logger.Warn("read pause exit conditions failed", "error", err)
		return
	}

	type transition struct {
		projectID string
		detail    string
	}
	transitions := make([]transition, 0)
	now := deps.now().UTC()
	for index := range cfg.Projects {
		configuredProject := cfg.Projects[index]
		if !configuredProject.Paused {
			continue
		}
		var resolver connector.IssueReferenceResolver
		if strings.TrimSpace(configuredProject.PausedUntilIssue) != "" {
			candidate := deps.connectorFor(configuredProject.ID)
			if resolved, ok := candidate.(connector.IssueReferenceResolver); ok {
				resolver = resolved
			}
		}
		result, err := pause.Evaluate(ctx, configuredProject, now, deps.repositoryFor(configuredProject.ID), resolver)
		if err != nil {
			deps.logger.Warn(
				"check project pause exit condition failed",
				"project_id", configuredProject.ID,
				"error", err,
			)
			continue
		}
		if !result.Met {
			continue
		}
		clearProjectPause(&cfg.Projects[index])
		transitions = append(transitions, transition{projectID: configuredProject.ID, detail: result.Detail})
	}
	if len(transitions) == 0 {
		return
	}
	if err := deps.write(cfg); err != nil {
		deps.logger.Warn("write automatic project unpause failed", "error", err)
		return
	}
	for _, item := range transitions {
		deps.logger.Info(
			"project automatically unpaused",
			"project_id", item.projectID,
			"exit_condition", item.detail,
		)
	}
}

func normalizePauseMonitorDeps(deps pauseMonitorDeps) pauseMonitorDeps {
	if deps.read == nil {
		deps.read = func() (globalconfig.Config, error) {
			return globalconfig.Config{}, errors.New("pause monitor config reader is required")
		}
	}
	if deps.write == nil {
		deps.write = func(globalconfig.Config) error {
			return errors.New("pause monitor config writer is required")
		}
	}
	if deps.connectorFor == nil {
		deps.connectorFor = func(string) connector.Connector {
			return nil
		}
	}
	if deps.repositoryFor == nil {
		deps.repositoryFor = func(string) string {
			return ""
		}
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.interval <= 0 {
		deps.interval = defaultPauseExitPollInterval
	}
	if deps.logger == nil {
		deps.logger = slog.Default()
	}
	return deps
}
