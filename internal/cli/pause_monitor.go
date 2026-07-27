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
	unpause       func(context.Context, string) error
	pause         func(context.Context, string) error
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
		pause     globalconfig.Project
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
		transitions = append(transitions, transition{
			projectID: configuredProject.ID,
			detail:    result.Detail,
			pause:     configuredProject,
		})
	}
	for _, item := range transitions {
		if !commitAutomaticUnpause(ctx, deps, item.projectID, item.pause) {
			continue
		}
		deps.logger.Info(
			"project automatically unpaused",
			"project_id", item.projectID,
			"exit_condition", item.detail,
		)
	}
}

func commitAutomaticUnpause(
	ctx context.Context,
	deps pauseMonitorDeps,
	projectID string,
	evaluated globalconfig.Project,
) bool {
	latest, err := deps.read()
	if err != nil {
		deps.logger.Warn("re-read automatic project unpause failed", "project_id", projectID, "error", err)
		return false
	}
	index := projectIndex(latest.Projects, projectID)
	if index < 0 || !sameProjectPause(latest.Projects[index], evaluated) {
		return false
	}

	if err := deps.unpause(ctx, projectID); err != nil {
		deps.logger.Warn("apply automatic project unpause failed", "project_id", projectID, "error", err)
		return false
	}

	latest, err = deps.read()
	if err != nil {
		rollbackAutomaticUnpause(ctx, deps, projectID, err)
		return false
	}
	index = projectIndex(latest.Projects, projectID)
	if index < 0 {
		return false
	}
	if !sameProjectPause(latest.Projects[index], evaluated) {
		if latest.Projects[index].Paused {
			rollbackAutomaticUnpause(ctx, deps, projectID, errors.New("pause condition changed during automatic unpause"))
		}
		return false
	}

	clearProjectPause(&latest.Projects[index])
	if err := deps.write(latest); err != nil {
		rollbackAutomaticUnpause(ctx, deps, projectID, err)
		return false
	}
	return true
}

func sameProjectPause(current globalconfig.Project, evaluated globalconfig.Project) bool {
	return current.Paused == evaluated.Paused &&
		current.PausedReason == evaluated.PausedReason &&
		current.PausedAt == evaluated.PausedAt &&
		current.PausedUntilIssue == evaluated.PausedUntilIssue &&
		current.PausedUntil == evaluated.PausedUntil
}

func rollbackAutomaticUnpause(ctx context.Context, deps pauseMonitorDeps, projectID string, cause error) {
	if err := deps.pause(ctx, projectID); err != nil {
		deps.logger.Error(
			"rollback automatic project unpause failed",
			"project_id", projectID,
			"error", errors.Join(cause, err),
		)
		return
	}
	deps.logger.Warn("automatic project unpause rolled back", "project_id", projectID, "error", cause)
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
	if deps.unpause == nil {
		deps.unpause = func(context.Context, string) error {
			return errors.New("pause monitor runtime unpause is required")
		}
	}
	if deps.pause == nil {
		deps.pause = func(context.Context, string) error {
			return errors.New("pause monitor runtime pause is required")
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
