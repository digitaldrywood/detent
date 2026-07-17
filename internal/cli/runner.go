package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/boardsnapshot"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/claudecode"
	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/projectcolor"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	defaultSnapshotInterval      = time.Second
	defaultBoardSnapshotInterval = 30 * time.Second
	defaultTokenTrendWindowSize  = 60
	defaultTokenThroughputWindow = time.Minute
)

type lifetimeTotalsSource interface {
	LifetimeTotals(context.Context) (store.LifetimeTotals, error)
}

type autoUpdateStatusSource interface {
	Status() detentupdate.AutoStatus
}

// withRunnerFactory returns a project.Factory that constructs a
// per-project agent Runner from the project's own workflow (so each project's
// codex command and workspace root are honored), injects it into the project's
// dependencies, and then delegates to load.
//
// If load is nil, the default project.Load is used.
func withRunnerFactory(
	deps project.Dependencies,
	sessionStore runnerpkg.SessionStore,
	load func(project.Dependencies) (*project.Project, error),
	githubTokenSource ...func() string,
) project.Factory {
	return func(cfg globalconfig.Project) (*project.Project, error) {
		workflow, err := project.LoadWorkflow(cfg)
		if err != nil {
			return nil, fmt.Errorf("load project workflow %s: %w", cfg.ID, err)
		}
		if cfg.Identity.Configured() {
			workflow.Config.Identity = cfg.Identity
			workflow.Config.Identity.Normalize()
		}

		run := deps.Runner
		if run == nil {
			var err error
			run, err = buildRunner(workflow, cfg.ID, cfg.Workdir, sessionStore, deps.Logger)
			if err != nil {
				return nil, fmt.Errorf("build project runner %s: %w", cfg.ID, err)
			}
		}

		projectDeps := deps
		projectDeps.Runner = run
		if len(githubTokenSource) > 0 && githubTokenSource[0] != nil {
			projectDeps.GitHubToken = githubTokenSource[0]()
		}

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
	projectID string,
	projectWorkdir string,
	sessionStore runnerpkg.SessionStore,
	logger *slog.Logger,
) (orchestrator.Runner, error) {
	cfg := workflow.Config

	backend, err := buildWorkspaceBackend(cfg, projectWorkdir, logger)
	if err != nil {
		return nil, err
	}

	pricing, err := budget.PricingForConfig(budget.Config{
		PricingPath: cfg.Budget.PricingPath,
	})
	if err != nil {
		return nil, fmt.Errorf("load pricing: %w", err)
	}
	budgetGuardBuilder := func(cfg workflowconfig.Budget) (runnerpkg.BudgetChecker, runnerpkg.DispatchEstimator, error) {
		return buildBudgetDispatchGuards(projectID, cfg, sessionStore, pricing)
	}

	run, err := runnerpkg.NewRunner(runnerpkg.Dependencies{
		ProjectID:           projectID,
		Workflow:            workflow,
		Workspace:           backend,
		AgentBackendFactory: runnerpkg.AgentBackendFactoryFunc(buildAgentBackend),
		Store:               sessionStore,
		Pricing:             pricing,
		BudgetGuardBuilder:  budgetGuardBuilder,
		Logger:              logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	return run, nil
}

func buildBudgetDispatchGuards(
	projectID string,
	cfg workflowconfig.Budget,
	sessionStore runnerpkg.SessionStore,
	pricing budget.PricingTable,
) (runnerpkg.BudgetChecker, runnerpkg.DispatchEstimator, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	checkerConfig := budget.Config{
		ProjectID:       projectID,
		BillingMode:     cfg.EffectiveBillingMode(),
		Enabled:         cfg.Enabled,
		PerDayMaxUSD:    cfg.PerDayMaxUSD,
		PerIssueMaxUSD:  cfg.PerIssueMaxUSD,
		RefusalCooldown: time.Duration(cfg.RefusalCooldownSeconds) * time.Second,
		PricingPath:     cfg.PricingPath,
		Overrides:       budgetOverrideStore(sessionStore),
	}
	if cfg.EffectiveBillingMode() == workflowconfig.BillingModeSubscription {
		return budget.NewChecker(checkerConfig, nil, pricing), nil, nil
	}

	spendStore, ok := sessionStore.(budget.SpendStore)
	if !ok {
		return nil, nil, budget.ErrMissingSpendStore
	}
	checker := budget.NewChecker(checkerConfig, spendStore, pricing)

	estimateStore, ok := sessionStore.(budget.DispatchEstimateStore)
	if !ok {
		return checker, nil, nil
	}
	return checker, budget.NewDispatchEstimator(estimateStore), nil
}

func budgetOverrideStore(value any) budget.OverrideStore {
	store, ok := value.(budget.OverrideStore)
	if !ok {
		return nil
	}
	return store
}

func buildWorkspaceBackend(cfg workflowconfig.Config, sourceRootFallback string, logger *slog.Logger) (workspace.Backend, error) {
	root := strings.TrimSpace(cfg.Workspace.Root)
	sourceRoot := strings.TrimSpace(cfg.Workspace.SourceRoot)
	if sourceRoot == "" {
		sourceRoot = strings.TrimSpace(sourceRootFallback)
	}
	if sourceRoot == "" {
		sourceRoot = root
	}
	workspaceKind := strings.TrimSpace(cfg.Workspace.Kind)
	if workspaceKind == "" {
		workspaceKind = workspace.KindLocalGit
	}
	outputRoot := strings.TrimSpace(cfg.Workspace.OutputRoot)
	if outputRoot == "" {
		outputRoot = strings.TrimSpace(cfg.Deliverable.OutputRoot)
	}
	backend, err := workspace.NewBackend(workspaceKind, workspace.LocalGitOptions{
		Root:       root,
		SourceRoot: sourceRoot,
		OutputRoot: outputRoot,
		AutoBranch: cfg.Workspace.AutoBranch,
		Hooks: workspace.Hooks{
			Shell:        cfg.Hooks.Shell,
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

func buildAgentBackend(backend workflowconfig.AgentBackend) (runnerpkg.AgentBackend, error) {
	switch backend.Kind {
	case workflowconfig.AgentBackendCodex:
		return buildCodexAgentBackend(backend.Command, backend.CodexOptions())
	case workflowconfig.AgentBackendClaudeCode:
		return buildClaudeAgentBackend(backend.Command, backend.ClaudeCodeOptions())
	default:
		return nil, fmt.Errorf("unsupported agent backend kind %q; supported kinds: %s, %s",
			backend.Kind,
			workflowconfig.AgentBackendCodex,
			workflowconfig.AgentBackendClaudeCode,
		)
	}
}

func buildClaudeAgentBackend(command string, cfg workflowconfig.ClaudeCodeOptions) (runnerpkg.AgentBackend, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("claude command is required")
	}

	backend, err := claudecode.NewAgentBackend(claudecode.Options{
		CommandFactoryWithArgs: func(ctx context.Context, args []string) *exec.Cmd {
			return buildClaudeCommandFromConfig(ctx, command, cfg.Shell, args)
		},
		PermissionMode:         cfg.PermissionMode,
		Effort:                 cfg.Effort,
		AllowedTools:           cfg.AllowedTools,
		DisallowedTools:        cfg.DisallowedTools,
		IncludePartialMessages: cfg.IncludePartialMessages,
		ExtraArgs:              cfg.ExtraArgs,
		TurnTimeout:            durationFromMillis(cfg.TurnTimeoutMS),
		StallTimeout:           durationFromMillis(cfg.StallTimeoutMS),
	})
	if err != nil {
		return nil, fmt.Errorf("create claude backend: %w", err)
	}
	return backend, nil
}

func buildCodexAgentBackend(command string, cfg workflowconfig.CodexOptions) (runnerpkg.AgentBackend, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("codex command is required")
	}

	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return buildCodexCommandFromConfig(ctx, command, cfg.Shell)
	})
	if err != nil {
		return nil, fmt.Errorf("create codex transport factory: %w", err)
	}

	opts := []codex.AppServerOption{}
	if timeout := durationFromMillis(cfg.ReadTimeoutMS); timeout > 0 {
		opts = append(opts, codex.WithReadTimeout(timeout))
	}
	if timeout := durationFromMillis(cfg.TurnTimeoutMS); timeout > 0 {
		opts = append(opts, codex.WithTurnTimeout(timeout))
	}

	client, err := codex.NewAppServer(factory, opts...)
	if err != nil {
		return nil, fmt.Errorf("create codex app-server: %w", err)
	}
	backend, err := codex.NewAgentBackend(client, codex.OptionsFromConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("create codex backend: %w", err)
	}
	return backend, nil
}

func buildCodexCommand(ctx context.Context, cfg workflowconfig.Config) *exec.Cmd {
	return buildCodexCommandFromConfig(ctx, cfg.Codex.Command, cfg.Codex.Shell)
}

func buildCodexCommandFromConfig(ctx context.Context, command string, shell string) *exec.Cmd {
	return commandshell.Command(ctx, strings.TrimSpace(command), shell)
}

func buildClaudeCommandFromConfig(ctx context.Context, command string, shell string, args []string) *exec.Cmd {
	return commandshell.CommandWithArgs(ctx, strings.TrimSpace(command), shell, args)
}

// publishSnapshots ticks at interval, building a merged telemetry snapshot
// across every running project's orchestrator and publishing it to hub until
// ctx is cancelled.
func publishSnapshots(
	ctx context.Context,
	registry *project.Registry,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	seq *atomic.Uint64,
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
	interval time.Duration,
	now func() time.Time,
	updateSources ...autoUpdateStatusSource,
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
	if seq == nil {
		seq = &atomic.Uint64{}
	}

	trend := newTokenTrendRecorder(defaultTokenTrendWindowSize)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := publishSnapshotOnce(ctx, registry, snapshotHub, seq, now(), trend, lifetimeSource, dashboardURL, updateSources...); err != nil {
			slog.Default().Warn("publish telemetry snapshot failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func republishSnapshotsOnProjectEvents(
	ctx context.Context,
	events *hub.Hub[project.Event],
	snapshotHub *hub.Hub[telemetry.Snapshot],
	logger *slog.Logger,
) {
	if events == nil || snapshotHub == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	sub, err := events.Subscribe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("subscribe project events for snapshot republish failed", "error", err)
		}
		return
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sub.C():
			if !ok {
				return
			}
			republishLatestSnapshot(snapshotHub, logger)
		}
	}
}

func republishLatestSnapshot(snapshotHub *hub.Hub[telemetry.Snapshot], logger *slog.Logger) {
	if snapshotHub == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := snapshotHub.Republish(); err != nil {
		logger.Warn("republish telemetry snapshot failed", "error", err)
	}
}

func persistBoardSnapshots(
	ctx context.Context,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	cache boardsnapshot.Store,
	interval time.Duration,
	logger *slog.Logger,
) {
	if snapshotHub == nil || cache == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = defaultBoardSnapshotInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	subscription, err := snapshotHub.Subscribe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("subscribe board snapshot cache failed", "error", err)
		}
		return
	}
	defer subscription.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var pending telemetry.Snapshot
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-subscription.C():
			if !ok {
				return
			}
			if boardsnapshot.Eligible(snapshot) {
				pending = snapshot
				dirty = true
			}
		case <-ticker.C:
			if !dirty {
				continue
			}
			if err := cache.Save(ctx, pending); err != nil {
				logger.Warn("persist board snapshot failed", "error", err)
				continue
			}
			dirty = false
		}
	}
}

func publishStartupSnapshotOnce(
	ctx context.Context,
	cfg globalconfig.Config,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
	now time.Time,
	updateSources ...autoUpdateStatusSource,
) error {
	if snapshotHub == nil {
		return nil
	}
	snapshot := startupSnapshot(ctx, cfg, lifetimeSource, dashboardURL, now, updateSources...)
	if err := snapshotHub.Publish(snapshot); err != nil {
		return fmt.Errorf("publish startup snapshot: %w", err)
	}
	return nil
}

func startupSnapshot(
	ctx context.Context,
	cfg globalconfig.Config,
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
	now time.Time,
	updateSources ...autoUpdateStatusSource,
) telemetry.Snapshot {
	nextRefreshAt := now
	refresh := telemetry.Refresh{Status: telemetry.RefreshStatusInitializing, NextRefreshAt: &nextRefreshAt}
	snapshot := telemetry.Snapshot{
		GeneratedAt:    now,
		Instance:       startupSnapshotInstance(cfg),
		Projects:       startupProjectSnapshots(cfg.Projects, refresh),
		DashboardURL:   cleanDashboardURL(dashboardURL),
		Shutdown:       telemetry.Shutdown{Status: "running"},
		Refresh:        refresh,
		LifetimeTotals: lifetimeTotals(ctx, lifetimeSource),
		Update:         telemetryUpdateStatus(updateSources),
	}
	switch len(snapshot.Projects) {
	case 0:
	case 1:
		snapshot.Project = snapshot.Projects[0].Project
	default:
		snapshot.Project = telemetry.Project{DisplayName: "multiple projects"}
	}
	return snapshot
}

func startupSnapshotInstance(cfg globalconfig.Config) telemetry.Instance {
	identity := cfg.Global.Identity
	identity.Normalize()
	return telemetry.Instance{
		Name:        identity.Name,
		GitHubLogin: identity.GitHubLogin,
	}
}

func startupProjectSnapshots(projects []globalconfig.Project, refresh telemetry.Refresh) []telemetry.ProjectSnapshot {
	out := make([]telemetry.ProjectSnapshot, 0, len(projects))
	for _, cfg := range projects {
		id := strings.TrimSpace(cfg.ID)
		if id == "" {
			continue
		}
		out = append(out, telemetry.ProjectSnapshot{
			Project: telemetry.Project{
				ID:          id,
				DisplayName: id,
				Color:       projectcolor.ColorFor(id, cfg.Color),
			},
			Refresh: refresh,
		})
	}
	return out
}

func publishSnapshotOnce(
	ctx context.Context,
	registry *project.Registry,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	seq *atomic.Uint64,
	now time.Time,
	trend *tokenTrendRecorder,
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
	updateSources ...autoUpdateStatusSource,
) error {
	merged := telemetry.Snapshot{GeneratedAt: now}
	trackedProjects := registry.List()
	if len(trackedProjects) == 0 {
		return nil
	}
	for _, trackedProject := range trackedProjects {
		projectMetadata := projectSnapshotMetadata(trackedProject)
		if !trackedProject.Running() {
			if trackedProject.Paused() {
				merged = mergeSnapshot(merged, telemetry.Snapshot{
					Project:      projectMetadata,
					DashboardURL: cleanDashboardURL(dashboardURL),
					Shutdown:     telemetry.Shutdown{Status: "running"},
				})
				continue
			}
			if runtimeErr := trackedProject.RuntimeError(); runtimeErr.Message != "" {
				lastErrorAt := runtimeErr.At
				merged = mergeSnapshot(merged, telemetry.Snapshot{
					Project:      projectMetadata,
					DashboardURL: cleanDashboardURL(dashboardURL),
					Shutdown:     telemetry.Shutdown{Status: "running"},
					Refresh: telemetry.Refresh{
						Status:      telemetry.RefreshStatusDegraded,
						LastError:   runtimeErr.Message,
						LastErrorAt: &lastErrorAt,
					},
				})
				continue
			}
			nextRefreshAt := now
			refresh := telemetry.Refresh{Status: telemetry.RefreshStatusInitializing, NextRefreshAt: &nextRefreshAt}
			merged = mergeSnapshot(merged, telemetry.Snapshot{
				Project:      projectMetadata,
				DashboardURL: cleanDashboardURL(dashboardURL),
				Shutdown:     telemetry.Shutdown{Status: "running"},
				Refresh:      refresh,
			})
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}
		state, err := orch.State(ctx)
		if err != nil {
			slog.Default().Warn(
				"project telemetry snapshot skipped",
				slog.String("project_id", string(trackedProject.ID())),
				slog.String("error", err.Error()),
			)
			lastErrorAt := now
			merged = mergeSnapshot(merged, telemetry.Snapshot{
				Project:      projectMetadata,
				DashboardURL: cleanDashboardURL(dashboardURL),
				Shutdown:     telemetry.Shutdown{Status: "running"},
				Refresh: telemetry.Refresh{
					Status:      telemetry.RefreshStatusDegraded,
					LastError:   err.Error(),
					LastErrorAt: &lastErrorAt,
				},
			})
			continue
		}
		snapshot := state.Snapshot(now)
		snapshot.Project = projectMetadata
		snapshot.DashboardURL = cleanDashboardURL(dashboardURL)
		merged = mergeSnapshot(merged, snapshot)
	}
	merged = dedupeSnapshotIssues(merged)
	if trend != nil {
		merged = trend.apply(merged)
	}
	merged.LifetimeTotals = lifetimeTotals(ctx, lifetimeSource)
	merged.Update = telemetryUpdateStatus(updateSources)
	merged.Seq = seq.Add(1)
	if current, ok := snapshotHub.Latest(); ok && current.LastKnown && now.Before(current.LastKnownUntil) && !boardsnapshot.Eligible(merged) {
		return nil
	}
	if err := snapshotHub.Publish(merged); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	return nil
}

func telemetryUpdateStatus(sources []autoUpdateStatusSource) telemetry.Update {
	if len(sources) == 0 || sources[0] == nil {
		return telemetry.Update{}
	}
	status := sources[0].Status()
	return telemetry.Update{
		Enabled:            status.Enabled,
		AutoApplyEnabled:   status.AutoApplyEnabled,
		CheckIntervalHours: int(status.CheckInterval / time.Hour),
		State:              status.State,
		LastCheckAt:        status.LastCheckAt,
		LastAppliedVersion: status.LastAppliedVersion,
		NextCheckAt:        status.NextCheckAt,
		AvailableVersion:   status.AvailableVersion,
		LastError:          status.LastError,
	}
}

func projectSnapshotMetadata(trackedProject *project.Project) telemetry.Project {
	if trackedProject == nil {
		return telemetry.Project{}
	}

	cfg := trackedProject.Config()
	workflow := trackedProject.Workflow()
	id := strings.TrimSpace(cfg.ID)
	return telemetry.Project{
		ID:          id,
		DisplayName: id,
		URL:         projectURLFromWorkflow(workflow.Config),
		Color:       projectcolor.ColorFor(id, cfg.Color),
	}
}

func projectURLFromWorkflow(cfg workflowconfig.Config) string {
	slug := strings.TrimSpace(cfg.Tracker.ProjectSlug)
	if strings.HasPrefix(slug, "http://") || strings.HasPrefix(slug, "https://") {
		return slug
	}
	return ""
}

func cleanDashboardURL(value string) string {
	return strings.TrimSpace(value)
}

type tokenTrendRecorder struct {
	limit  int
	window time.Duration
	points []telemetry.TokenTrendPoint
}

func newTokenTrendRecorder(limit int) *tokenTrendRecorder {
	if limit <= 0 {
		limit = defaultTokenTrendWindowSize
	}
	return &tokenTrendRecorder{limit: limit, window: defaultTokenThroughputWindow}
}

func (r *tokenTrendRecorder) apply(snapshot telemetry.Snapshot) telemetry.Snapshot {
	if snapshot.Tokens.Input > 0 || snapshot.Tokens.Output > 0 || snapshot.Tokens.Total > 0 {
		total := snapshot.Tokens.Total
		if total <= 0 {
			total = snapshot.Tokens.Input + snapshot.Tokens.Output
		}
		point := telemetry.TokenTrendPoint{
			At:     snapshot.GeneratedAt,
			Input:  snapshot.Tokens.Input,
			Output: snapshot.Tokens.Output,
			Total:  total,
		}
		if r.shouldReset(point) {
			r.points = nil
		}
		r.points = append(r.points, point)
		if len(r.points) > r.limit {
			r.points = append([]telemetry.TokenTrendPoint(nil), r.points[len(r.points)-r.limit:]...)
		}
	} else {
		r.points = nil
	}
	snapshot.TokenTrend = append([]telemetry.TokenTrendPoint(nil), r.points...)
	snapshot.Throughput = r.throughput()
	return snapshot
}

func (r *tokenTrendRecorder) shouldReset(point telemetry.TokenTrendPoint) bool {
	if len(r.points) == 0 {
		return false
	}
	latest := r.points[len(r.points)-1]
	return point.Total < latest.Total || !point.At.After(latest.At)
}

func (r *tokenTrendRecorder) throughput() telemetry.TokenThroughput {
	window := r.window
	if window <= 0 {
		window = defaultTokenThroughputWindow
	}

	throughput := telemetry.TokenThroughput{WindowSeconds: int64(window / time.Second)}
	if len(r.points) < 2 {
		return throughput
	}

	latest := r.points[len(r.points)-1]
	windowStart := latest.At.Add(-window)
	base := latest
	for _, point := range r.points[:len(r.points)-1] {
		if point.At.Before(windowStart) {
			continue
		}
		base = point
		break
	}

	elapsed := latest.At.Sub(base.At).Seconds()
	if elapsed <= 0 {
		return throughput
	}

	tokens := latest.Total - base.Total
	if tokens <= 0 {
		return throughput
	}

	throughput.Tokens = tokens
	throughput.TokensPerSecond = float64(tokens) / elapsed
	return throughput
}

func lifetimeTotals(ctx context.Context, source lifetimeTotalsSource) telemetry.LifetimeTotals {
	if source == nil {
		return telemetry.LifetimeTotals{DegradedReason: "runtime store unavailable"}
	}
	totals, err := source.LifetimeTotals(ctx)
	if err != nil {
		return telemetry.LifetimeTotals{DegradedReason: "read runtime store lifetime totals: " + err.Error()}
	}
	return telemetry.LifetimeTotals{
		Available:             true,
		InputTokens:           totals.InputTokens,
		CachedInputTokens:     totals.CachedInputTokens,
		OutputTokens:          totals.OutputTokens,
		ReasoningOutputTokens: totals.ReasoningOutputTokens,
		TotalTokens:           totals.TotalTokens,
		RuntimeSeconds:        totals.RuntimeSeconds,
		Sessions:              totals.Sessions,
		Runs:                  totals.Runs,
		OrphanResumed:         totals.OrphanResumed,
		OrphanFresh:           totals.OrphanFresh,
		ResumedInputTokens:    totals.ResumedInputTokens,
		ResumedCachedTokens:   totals.ResumedCachedTokens,
	}
}

func mergeSnapshot(current, next telemetry.Snapshot) telemetry.Snapshot {
	next = stampSnapshotProjectID(next)
	current.Project = mergeProject(current.Project, next.Project)
	current.Instance = mergeInstance(current.Instance, next.Instance)
	if project := projectSnapshot(next); project.Project != (telemetry.Project{}) {
		current.Projects = append(current.Projects, project)
	}
	if strings.TrimSpace(current.DashboardURL) == "" {
		current.DashboardURL = next.DashboardURL
	}
	current.Refresh = mergeRefresh(current.Refresh, next.Refresh)
	current.Shutdown = mergeShutdown(current.Shutdown, next.Shutdown)

	current.Running = append(current.Running, next.Running...)
	current.WorkAttempts = append(current.WorkAttempts, next.WorkAttempts...)
	current.SchedulerDecisions = append(current.SchedulerDecisions, next.SchedulerDecisions...)
	if !next.Release.IsZero() {
		current.Releases = append(current.Releases, next.Release)
		if current.Release.IsZero() {
			current.Release = next.Release
		}
	}
	current.Queue = append(current.Queue, next.Queue...)
	current.Blocked = append(current.Blocked, next.Blocked...)
	current.Completed = append(current.Completed, next.Completed...)
	current.BoardIssues = append(current.BoardIssues, next.BoardIssues...)
	current.Pipeline = append(current.Pipeline, next.Pipeline...)
	current.TrackerDrift.UntrackedOpen = append(current.TrackerDrift.UntrackedOpen, next.TrackerDrift.UntrackedOpen...)
	current.TrackerDrift.OpenTerminal = append(current.TrackerDrift.OpenTerminal, next.TrackerDrift.OpenTerminal...)
	current.Budget.Refusals = append(current.Budget.Refusals, next.Budget.Refusals...)
	current.BackendOutages = append(current.BackendOutages, next.BackendOutages...)
	current.FailureBreakers = append(current.FailureBreakers, next.FailureBreakers...)
	current.DispatchRecoveries = append(current.DispatchRecoveries, next.DispatchRecoveries...)
	current.OverloadRetriesLastHour += next.OverloadRetriesLastHour

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

func issueKey(issue telemetry.Issue) string {
	for _, value := range []string{issue.URL, issue.Identifier, issue.ID} {
		if key := strings.TrimSpace(value); key != "" {
			return key
		}
	}
	return ""
}

func dedupeSnapshotIssues(snapshot telemetry.Snapshot) telemetry.Snapshot {
	var removed int
	snapshot.Running, removed = dedupeIssueRows(snapshot.Running, func(row telemetry.Running) telemetry.Issue { return row.Issue })
	snapshot.Counts.Running = dedupedCount(snapshot.Counts.Running, removed, len(snapshot.Running))
	snapshot.Queue, removed = dedupeIssueRows(snapshot.Queue, func(row telemetry.Queued) telemetry.Issue { return row.Issue })
	snapshot.Counts.Queue = dedupedCount(snapshot.Counts.Queue, removed, len(snapshot.Queue))
	snapshot.Blocked, removed = dedupeIssueRows(snapshot.Blocked, func(row telemetry.Blocked) telemetry.Issue { return row.Issue })
	snapshot.Counts.Blocked = dedupedCount(snapshot.Counts.Blocked, removed, len(snapshot.Blocked))
	snapshot.Completed, removed = dedupeCompleted(snapshot.Completed)
	snapshot.Counts.Completed = dedupedCount(snapshot.Counts.Completed, removed, len(snapshot.Completed))
	return snapshot
}

func dedupeIssueRows[T any](rows []T, issue func(T) telemetry.Issue) ([]T, int) {
	seen := make(map[string]int, len(rows))
	deduped := make([]T, 0, len(rows))
	for _, row := range rows {
		key := issueKey(issue(row))
		if key == "" {
			deduped = append(deduped, row)
			continue
		}
		_, ok := seen[key]
		if !ok {
			seen[key] = len(deduped)
			deduped = append(deduped, row)
			continue
		}
	}
	return deduped, len(rows) - len(deduped)
}

func dedupeCompleted(rows []telemetry.Completed) ([]telemetry.Completed, int) {
	latest := make(map[string]int, len(rows))
	for i, row := range rows {
		key := issueKey(row.Issue)
		if key == "" {
			continue
		}
		current, ok := latest[key]
		if !ok || row.CompletedAt.After(rows[current].CompletedAt) {
			latest[key] = i
		}
	}

	deduped := make([]telemetry.Completed, 0, len(latest))
	for i, row := range rows {
		key := issueKey(row.Issue)
		if key == "" || latest[key] == i {
			deduped = append(deduped, row)
		}
	}
	return deduped, len(rows) - len(deduped)
}

func dedupedCount(count, removed, length int) int {
	count -= removed
	if count < length {
		return length
	}
	return count
}

func stampSnapshotProjectID(snapshot telemetry.Snapshot) telemetry.Snapshot {
	projectID := strings.TrimSpace(snapshot.Project.ID)
	if projectID == "" {
		return snapshot
	}
	if !snapshot.Release.IsZero() && strings.TrimSpace(snapshot.Release.ProjectID) == "" {
		snapshot.Release.ProjectID = projectID
	}
	for i := range snapshot.Refresh.Sources {
		if strings.TrimSpace(snapshot.Refresh.Sources[i].ProjectID) == "" {
			snapshot.Refresh.Sources[i].ProjectID = projectID
		}
	}
	if len(snapshot.Refresh.Sources) == 0 && snapshot.Refresh.Degraded() {
		snapshot.Refresh.Sources = []telemetry.RefreshSource{{
			ProjectID:     projectID,
			Name:          telemetry.RefreshSourceProject,
			LastSuccessAt: cloneTime(snapshot.Refresh.LastRefreshAt),
			Degraded:      true,
			LastError:     snapshot.Refresh.LastError,
			LastErrorAt:   cloneTime(snapshot.Refresh.LastErrorAt),
		}}
	}

	for i := range snapshot.Pipeline {
		snapshot.Pipeline[i] = stampIssueProjectID(snapshot.Pipeline[i], projectID)
	}
	for i := range snapshot.BoardIssues {
		snapshot.BoardIssues[i] = stampIssueProjectID(snapshot.BoardIssues[i], projectID)
	}
	for i := range snapshot.TrackerDrift.UntrackedOpen {
		snapshot.TrackerDrift.UntrackedOpen[i] = stampIssueProjectID(snapshot.TrackerDrift.UntrackedOpen[i], projectID)
	}
	for i := range snapshot.TrackerDrift.OpenTerminal {
		snapshot.TrackerDrift.OpenTerminal[i] = stampIssueProjectID(snapshot.TrackerDrift.OpenTerminal[i], projectID)
	}
	for i := range snapshot.Running {
		snapshot.Running[i].Issue = stampIssueProjectID(snapshot.Running[i].Issue, projectID)
	}
	for i := range snapshot.Queue {
		snapshot.Queue[i].Issue = stampIssueProjectID(snapshot.Queue[i].Issue, projectID)
	}
	for i := range snapshot.Blocked {
		snapshot.Blocked[i].Issue = stampIssueProjectID(snapshot.Blocked[i].Issue, projectID)
	}
	for i := range snapshot.Completed {
		snapshot.Completed[i].Issue = stampIssueProjectID(snapshot.Completed[i].Issue, projectID)
	}
	for i := range snapshot.WorkAttempts {
		if strings.TrimSpace(snapshot.WorkAttempts[i].ProjectID) == "" {
			snapshot.WorkAttempts[i].ProjectID = projectID
		}
	}
	for i := range snapshot.SchedulerDecisions {
		if strings.TrimSpace(snapshot.SchedulerDecisions[i].ProjectID) == "" {
			snapshot.SchedulerDecisions[i].ProjectID = projectID
		}
	}
	for i := range snapshot.BackendOutages {
		if strings.TrimSpace(snapshot.BackendOutages[i].ProjectID) == "" {
			snapshot.BackendOutages[i].ProjectID = projectID
		}
	}
	for i := range snapshot.FailureBreakers {
		if strings.TrimSpace(snapshot.FailureBreakers[i].ProjectID) == "" {
			snapshot.FailureBreakers[i].ProjectID = projectID
		}
	}
	for i := range snapshot.DispatchRecoveries {
		if strings.TrimSpace(snapshot.DispatchRecoveries[i].ProjectID) == "" {
			snapshot.DispatchRecoveries[i].ProjectID = projectID
		}
	}
	return snapshot
}

func stampIssueProjectID(issue telemetry.Issue, projectID string) telemetry.Issue {
	if strings.TrimSpace(issue.ProjectID) == "" {
		issue.ProjectID = projectID
	}
	return issue
}

func mergeShutdown(current, next telemetry.Shutdown) telemetry.Shutdown {
	if next == (telemetry.Shutdown{}) {
		if current == (telemetry.Shutdown{}) {
			return telemetry.Shutdown{Status: "running"}
		}
		return current
	}
	if current == (telemetry.Shutdown{}) {
		current = telemetry.Shutdown{Status: "running"}
	}
	if strings.TrimSpace(next.Status) == "" {
		next.Status = "running"
	}
	if !current.Draining && !next.Draining {
		if current.Status == "" || current.Status == "running" {
			current.Status = next.Status
		}
		return current
	}

	current.Status = "draining"
	current.Draining = current.Draining || next.Draining
	current.SessionsRemaining += next.SessionsRemaining
	current.RequestedAt = earliestTime(current.RequestedAt, next.RequestedAt)
	current.CompletedAt = latestTime(current.CompletedAt, next.CompletedAt)
	if strings.TrimSpace(next.Result) != "" {
		current.Result = next.Result
	}
	return current
}

func projectSnapshot(snapshot telemetry.Snapshot) telemetry.ProjectSnapshot {
	return telemetry.ProjectSnapshot{
		Project:    snapshot.Project,
		Counts:     snapshot.Counts,
		Tokens:     snapshot.Tokens,
		Throughput: snapshot.Throughput,
		Auth:       snapshot.Auth,
		Refresh:    snapshot.Refresh,
	}
}

func mergeProject(current, next telemetry.Project) telemetry.Project {
	if current == (telemetry.Project{}) {
		return next
	}
	if next == (telemetry.Project{}) || current == next {
		return current
	}
	return telemetry.Project{DisplayName: "multiple projects"}
}

func mergeInstance(current, next telemetry.Instance) telemetry.Instance {
	if current == (telemetry.Instance{}) {
		return next
	}
	if next == (telemetry.Instance{}) || current == next {
		return current
	}
	return telemetry.Instance{
		Name:                    mergeInstanceValue(current.Name, next.Name, "multiple instances"),
		GitHubLogin:             mergeInstanceValue(current.GitHubLogin, next.GitHubLogin, "multiple logins"),
		AuthorizationScope:      mergeInstanceValue(current.AuthorizationScope, next.AuthorizationScope, "Multiple authorization scopes"),
		AuthorizationConfigured: current.AuthorizationConfigured || next.AuthorizationConfigured,
	}
}

func mergeInstanceValue(current, next string, mixed string) string {
	current = strings.TrimSpace(current)
	next = strings.TrimSpace(next)
	switch {
	case current == "":
		return next
	case next == "" || current == next:
		return current
	default:
		return mixed
	}
}

func mergeRefresh(current, next telemetry.Refresh) telemetry.Refresh {
	currentHadSignal := refreshHasSignal(current)
	nextHadSignal := refreshHasSignal(next)
	if current.PollIntervalSeconds == 0 ||
		(next.PollIntervalSeconds > 0 && next.PollIntervalSeconds < current.PollIntervalSeconds) {
		current.PollIntervalSeconds = next.PollIntervalSeconds
	}
	if current.StaleAfterSeconds == 0 ||
		(next.StaleAfterSeconds > 0 && next.StaleAfterSeconds < current.StaleAfterSeconds) {
		current.StaleAfterSeconds = next.StaleAfterSeconds
	}
	if current.FailureThreshold == 0 ||
		(next.FailureThreshold > 0 && next.FailureThreshold < current.FailureThreshold) {
		current.FailureThreshold = next.FailureThreshold
	}
	if next.DataSeq > current.DataSeq {
		current.DataSeq = next.DataSeq
	}
	current.LastRefreshAt = latestTime(current.LastRefreshAt, next.LastRefreshAt)
	current.NextRefreshAt = earliestTime(current.NextRefreshAt, next.NextRefreshAt)
	if strings.TrimSpace(next.LastError) != "" {
		if strings.TrimSpace(current.LastError) == "" ||
			current.LastErrorAt == nil ||
			next.LastErrorAt == nil ||
			current.LastErrorAt.Before(*next.LastErrorAt) {
			current.LastError = next.LastError
		}
	}
	current.LastErrorAt = latestTime(current.LastErrorAt, next.LastErrorAt)
	current.Sources = mergeRefreshSources(current.Sources, next.Sources)
	current.Manual = mergeRefreshAttempt(current.Manual, next.Manual)
	switch {
	case !currentHadSignal && nextHadSignal:
		current.Status = next.ReadinessStatus()
	case currentHadSignal && !nextHadSignal:
		current.Status = current.ReadinessStatus()
	case current.ReadinessStatus() == telemetry.RefreshStatusDegraded || next.ReadinessStatus() == telemetry.RefreshStatusDegraded:
		current.Status = telemetry.RefreshStatusDegraded
	case current.ReadinessStatus() == telemetry.RefreshStatusInitializing || next.ReadinessStatus() == telemetry.RefreshStatusInitializing:
		current.Status = telemetry.RefreshStatusInitializing
	case currentHadSignal || nextHadSignal:
		current.Status = telemetry.RefreshStatusReady
	default:
		current.Status = ""
	}
	return current
}

func refreshHasSignal(refresh telemetry.Refresh) bool {
	return refresh.PollIntervalSeconds != 0 ||
		refresh.StaleAfterSeconds != 0 ||
		refresh.FailureThreshold != 0 ||
		refresh.Status != "" ||
		refresh.LastRefreshAt != nil ||
		refresh.NextRefreshAt != nil ||
		strings.TrimSpace(refresh.LastError) != "" ||
		refresh.LastErrorAt != nil ||
		len(refresh.Sources) > 0 ||
		refresh.Manual != nil
}

func mergeRefreshSources(current []telemetry.RefreshSource, next []telemetry.RefreshSource) []telemetry.RefreshSource {
	merged := make([]telemetry.RefreshSource, 0, len(current)+len(next))
	index := make(map[string]int, len(current)+len(next))
	appendSource := func(source telemetry.RefreshSource) {
		key := strings.TrimSpace(source.ProjectID) + "\x00" + string(source.Name)
		if existing, ok := index[key]; ok {
			if refreshSourceObservedAt(source).After(refreshSourceObservedAt(merged[existing])) {
				merged[existing] = cloneRefreshSource(source)
			}
			return
		}
		index[key] = len(merged)
		merged = append(merged, cloneRefreshSource(source))
	}
	for _, source := range current {
		appendSource(source)
	}
	for _, source := range next {
		appendSource(source)
	}
	return merged
}

func refreshSourceObservedAt(source telemetry.RefreshSource) time.Time {
	if source.LastErrorAt != nil && (source.LastSuccessAt == nil || source.LastErrorAt.After(*source.LastSuccessAt)) {
		return source.LastErrorAt.UTC()
	}
	if source.LastSuccessAt != nil {
		return source.LastSuccessAt.UTC()
	}
	return time.Time{}
}

func cloneRefreshSource(source telemetry.RefreshSource) telemetry.RefreshSource {
	source.LastSuccessAt = cloneTime(source.LastSuccessAt)
	source.LastErrorAt = cloneTime(source.LastErrorAt)
	return source
}

func mergeRefreshAttempt(current *telemetry.RefreshAttempt, next *telemetry.RefreshAttempt) *telemetry.RefreshAttempt {
	if current == nil {
		return cloneRefreshAttemptPtr(next)
	}
	if next == nil {
		return cloneRefreshAttemptPtr(current)
	}
	if refreshAttemptRequestedAt(next).After(refreshAttemptRequestedAt(current)) {
		return cloneRefreshAttemptPtr(next)
	}
	return cloneRefreshAttemptPtr(current)
}

func refreshAttemptRequestedAt(attempt *telemetry.RefreshAttempt) time.Time {
	if attempt == nil || attempt.RequestedAt == nil {
		return time.Time{}
	}
	return attempt.RequestedAt.UTC()
}

func cloneRefreshAttemptPtr(attempt *telemetry.RefreshAttempt) *telemetry.RefreshAttempt {
	if attempt == nil {
		return nil
	}
	cloned := *attempt
	cloned.RequestedAt = cloneTime(attempt.RequestedAt)
	cloned.StartedAt = cloneTime(attempt.StartedAt)
	cloned.CompletedAt = cloneTime(attempt.CompletedAt)
	cloned.LastErrorAt = cloneTime(attempt.LastErrorAt)
	cloned.Operations = append([]string(nil), attempt.Operations...)
	return &cloned
}

func latestTime(current *time.Time, next *time.Time) *time.Time {
	switch {
	case current == nil:
		return cloneTime(next)
	case next == nil || current.After(*next):
		return cloneTime(current)
	default:
		return cloneTime(next)
	}
}

func earliestTime(current *time.Time, next *time.Time) *time.Time {
	switch {
	case current == nil:
		return cloneTime(next)
	case next == nil || current.Before(*next):
		return cloneTime(current)
	default:
		return cloneTime(next)
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func durationFromMillis(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
