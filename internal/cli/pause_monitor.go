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
	read             func() (globalconfig.Config, error)
	write            func(globalconfig.Config) error
	unpause          func(context.Context, string) error
	pause            func(context.Context, string) error
	connectorFor     func(string) connector.Connector
	repositoryFor    func(string) string
	trackerKindFor   func(string) string
	pauseStatus      func(string) (pause.ExitStatus, bool)
	setPauseStatus   func(pause.ExitStatus)
	clearPauseStatus func(string)
	now              func() time.Time
	interval         time.Duration
	logger           *slog.Logger
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
	trackers := pauseMonitorTrackers(cfg, deps)
	for index := range cfg.Projects {
		configuredProject := cfg.Projects[index]
		if !configuredProject.Paused {
			deps.clearPauseStatus(configuredProject.ID)
			continue
		}
		if strings.TrimSpace(configuredProject.PausedUntilIssue) == "" {
			deps.clearPauseStatus(configuredProject.ID)
		}
		var resolver connector.IssueReferenceResolver
		resolverProjectID := configuredProject.ID
		trackerRepository := deps.repositoryFor(configuredProject.ID)
		if strings.TrimSpace(configuredProject.PausedUntilIssue) != "" {
			resolution, resolveErr := pause.ResolveReference(configuredProject.ID, configuredProject.PausedUntilIssue, trackers)
			if resolveErr != nil {
				recordPauseEvaluationFailure(deps, configuredProject, "", now, resolveErr)
				deps.logger.Warn(
					"check project pause exit condition failed",
					"project_id", configuredProject.ID,
					"pause_exit_issue", configuredProject.PausedUntilIssue,
					"error", resolveErr,
				)
				continue
			}
			resolverProjectID = resolution.ProjectID
			trackerRepository = resolution.Repository
			candidate := deps.connectorFor(resolverProjectID)
			if resolved, ok := candidate.(connector.IssueReferenceResolver); ok {
				resolver = resolved
			}
		}
		result, err := pause.Evaluate(ctx, configuredProject, now, trackerRepository, resolver)
		if err != nil {
			recordPauseEvaluationFailure(deps, configuredProject, resolverProjectID, now, err)
			deps.logger.Warn(
				"check project pause exit condition failed",
				"project_id", configuredProject.ID,
				"pause_exit_issue", configuredProject.PausedUntilIssue,
				"resolver_project_id", resolverProjectID,
				"error", err,
			)
			continue
		}
		if strings.TrimSpace(configuredProject.PausedUntilIssue) != "" {
			deps.setPauseStatus(pause.ExitStatus{
				ProjectID:         configuredProject.ID,
				Reference:         strings.TrimSpace(configuredProject.PausedUntilIssue),
				ResolverProjectID: resolverProjectID,
				Evaluable:         true,
				EvaluatedAt:       now,
			})
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
		deps.clearPauseStatus(item.projectID)
		deps.logger.Info(
			"project automatically unpaused",
			"project_id", item.projectID,
			"exit_condition", item.detail,
		)
	}
}

func pauseMonitorTrackers(cfg globalconfig.Config, deps pauseMonitorDeps) []pause.Tracker {
	trackers := make([]pause.Tracker, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		trackers = append(trackers, pause.Tracker{
			ProjectID:  project.ID,
			Kind:       deps.trackerKindFor(project.ID),
			Repository: deps.repositoryFor(project.ID),
		})
	}
	return trackers
}

func recordPauseEvaluationFailure(
	deps pauseMonitorDeps,
	project globalconfig.Project,
	resolverProjectID string,
	now time.Time,
	err error,
) {
	failures := 1
	if previous, ok := deps.pauseStatus(project.ID); ok &&
		strings.EqualFold(strings.TrimSpace(previous.Reference), strings.TrimSpace(project.PausedUntilIssue)) {
		failures = previous.ConsecutiveFailures + 1
	}
	deps.setPauseStatus(pause.ExitStatus{
		ProjectID:           project.ID,
		Reference:           strings.TrimSpace(project.PausedUntilIssue),
		ResolverProjectID:   strings.TrimSpace(resolverProjectID),
		LastError:           err.Error(),
		ConsecutiveFailures: failures,
		NeedsAttention:      failures >= pause.DefaultEvaluationFailureThreshold,
		EvaluatedAt:         now,
	})
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
	if deps.trackerKindFor == nil {
		deps.trackerKindFor = func(string) string {
			return ""
		}
	}
	if deps.pauseStatus == nil {
		deps.pauseStatus = func(string) (pause.ExitStatus, bool) {
			return pause.ExitStatus{}, false
		}
	}
	if deps.setPauseStatus == nil {
		deps.setPauseStatus = func(pause.ExitStatus) {}
	}
	if deps.clearPauseStatus == nil {
		deps.clearPauseStatus = func(string) {}
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
