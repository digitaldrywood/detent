package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/digitaldrywood/symphony/internal/codex"
	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
	"github.com/digitaldrywood/symphony/internal/hub"
	"github.com/digitaldrywood/symphony/internal/orchestrator"
	"github.com/digitaldrywood/symphony/internal/project"
	runnerpkg "github.com/digitaldrywood/symphony/internal/runner"
	"github.com/digitaldrywood/symphony/internal/telemetry"
	"github.com/digitaldrywood/symphony/internal/workspace"
)

const defaultSnapshotInterval = time.Second

// withRunnerFactory returns a project.ProjectFactory that constructs a
// per-project agent Runner from the project's own workflow (so each project's
// codex command and workspace root are honored), injects it into the project's
// dependencies, and then delegates to load.
//
// If load is nil, the default project.Load is used.
func withRunnerFactory(
	deps project.Dependencies,
	sessionStore runnerpkg.SessionStore,
	load func(project.Dependencies) (*project.Project, error),
) project.ProjectFactory {
	return func(cfg globalconfig.Project) (*project.Project, error) {
		workflow, err := workflowconfig.LoadWorkflow(cfg.Workflow)
		if err != nil {
			return nil, fmt.Errorf("load project workflow %s: %w", cfg.ID, err)
		}

		run, err := buildRunner(workflow, sessionStore, deps.Logger)
		if err != nil {
			return nil, fmt.Errorf("build project runner %s: %w", cfg.ID, err)
		}

		projectDeps := deps
		projectDeps.Runner = run

		if load != nil {
			return load(projectDeps)
		}
		return project.Load(cfg, projectDeps)
	}
}

// buildRunner constructs the agent Runner for a single project's workflow,
// wiring its workspace backend, codex app-server client, and session store.
func buildRunner(
	workflow workflowconfig.Workflow,
	sessionStore runnerpkg.SessionStore,
	logger *slog.Logger,
) (orchestrator.Runner, error) {
	cfg := workflow.Config

	backend, err := buildWorkspaceBackend(cfg, logger)
	if err != nil {
		return nil, err
	}

	codexClient, err := buildCodexClient(cfg)
	if err != nil {
		return nil, err
	}

	run, err := runnerpkg.NewRunner(runnerpkg.Dependencies{
		Workflow:  workflow,
		Workspace: backend,
		Codex:     codexClient,
		Store:     sessionStore,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	return run, nil
}

func buildWorkspaceBackend(cfg workflowconfig.Config, logger *slog.Logger) (workspace.Backend, error) {
	root := strings.TrimSpace(cfg.Workspace.Root)
	sourceRoot := strings.TrimSpace(cfg.Workspace.SourceRoot)
	if sourceRoot == "" {
		sourceRoot = root
	}
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		Root:       root,
		SourceRoot: sourceRoot,
		AutoBranch: cfg.Workspace.AutoBranch,
		Hooks: workspace.Hooks{
			AfterCreate:  cfg.Hooks.AfterCreate,
			BeforeRun:    cfg.Hooks.BeforeRun,
			AfterRun:     cfg.Hooks.AfterRun,
			BeforeRemove: cfg.Hooks.BeforeRemove,
			Timeout:      durationFromMillis(cfg.Hooks.TimeoutMS),
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create workspace backend: %w", err)
	}
	return backend, nil
}

func buildCodexClient(cfg workflowconfig.Config) (runnerpkg.CodexClient, error) {
	command := strings.TrimSpace(cfg.Codex.Command)
	if command == "" {
		return nil, fmt.Errorf("codex command is required")
	}

	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		// #nosec G204 -- codex command is operator-supplied trusted workflow config.
		return exec.CommandContext(ctx, "sh", "-c", command)
	})
	if err != nil {
		return nil, fmt.Errorf("create codex transport factory: %w", err)
	}

	opts := []codex.AppServerOption{}
	if timeout := durationFromMillis(cfg.Codex.ReadTimeoutMS); timeout > 0 {
		opts = append(opts, codex.WithReadTimeout(timeout))
	}
	if timeout := durationFromMillis(cfg.Codex.TurnTimeoutMS); timeout > 0 {
		opts = append(opts, codex.WithTurnTimeout(timeout))
	}

	client, err := codex.NewAppServer(factory, opts...)
	if err != nil {
		return nil, fmt.Errorf("create codex app-server: %w", err)
	}
	return client, nil
}

// publishSnapshots ticks at interval, building a merged telemetry snapshot
// across every running project's orchestrator and publishing it to hub until
// ctx is cancelled.
func publishSnapshots(
	ctx context.Context,
	registry *project.Registry,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	interval time.Duration,
	now func() time.Time,
) {
	if registry == nil || snapshotHub == nil {
		return
	}
	if interval <= 0 {
		interval = defaultSnapshotInterval
	}
	if now == nil {
		now = time.Now
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := publishSnapshotOnce(ctx, registry, snapshotHub, now()); err != nil {
			slog.Default().Warn("publish telemetry snapshot failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func publishSnapshotOnce(
	ctx context.Context,
	registry *project.Registry,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	now time.Time,
) error {
	merged := telemetry.Snapshot{GeneratedAt: now}
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}
		state, err := orch.State(ctx)
		if err != nil {
			continue
		}
		merged = mergeSnapshot(merged, state.Snapshot(now))
	}
	if err := snapshotHub.Publish(merged); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	return nil
}

func mergeSnapshot(current, next telemetry.Snapshot) telemetry.Snapshot {
	current.Running = append(current.Running, next.Running...)
	current.Queue = append(current.Queue, next.Queue...)
	current.Blocked = append(current.Blocked, next.Blocked...)
	current.Completed = append(current.Completed, next.Completed...)
	current.Budget.Refusals = append(current.Budget.Refusals, next.Budget.Refusals...)

	current.Counts.Running += next.Counts.Running
	current.Counts.Queue += next.Counts.Queue
	current.Counts.Blocked += next.Counts.Blocked
	current.Counts.Completed += next.Counts.Completed

	current.Tokens.Input += next.Tokens.Input
	current.Tokens.Output += next.Tokens.Output
	current.Tokens.Total += next.Tokens.Total
	current.Tokens.RuntimeSeconds += next.Tokens.RuntimeSeconds

	if current.RateLimits == nil && next.RateLimits != nil {
		current.RateLimits = next.RateLimits
	}
	return current
}

func durationFromMillis(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
