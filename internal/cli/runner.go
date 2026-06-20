package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
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
	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	defaultSnapshotInterval      = time.Second
	defaultTokenTrendWindowSize  = 60
	defaultTokenThroughputWindow = time.Minute
)

type lifetimeTotalsSource interface {
	LifetimeTotals(context.Context) (store.LifetimeTotals, error)
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

	run, err := runnerpkg.NewRunner(runnerpkg.Dependencies{
		ProjectID:           projectID,
		Workflow:            workflow,
		Workspace:           backend,
		AgentBackendFactory: runnerpkg.AgentBackendFactoryFunc(buildAgentBackend),
		Store:               sessionStore,
		Pricing:             pricing,
		Logger:              logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	return run, nil
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
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		Root:       root,
		SourceRoot: sourceRoot,
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
		return buildCodexAgentBackend(backend.CodexConfig(workflowconfig.Codex{}))
	default:
		return nil, fmt.Errorf("unsupported agent backend kind %q", backend.Kind)
	}
}

func buildCodexAgentBackend(cfg workflowconfig.Codex) (runnerpkg.AgentBackend, error) {
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		return nil, fmt.Errorf("codex command is required")
	}

	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return buildCodexCommandFromConfig(ctx, cfg)
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
	backend, err := codex.NewAgentBackend(client)
	if err != nil {
		return nil, fmt.Errorf("create codex backend: %w", err)
	}
	return backend, nil
}

func buildCodexCommand(ctx context.Context, cfg workflowconfig.Config) *exec.Cmd {
	return buildCodexCommandFromConfig(ctx, cfg.Codex)
}

func buildCodexCommandFromConfig(ctx context.Context, cfg workflowconfig.Codex) *exec.Cmd {
	return commandshell.Command(ctx, strings.TrimSpace(cfg.Command), cfg.Shell)
}

// publishSnapshots ticks at interval, building a merged telemetry snapshot
// across every running project's orchestrator and publishing it to hub until
// ctx is cancelled.
func publishSnapshots(
	ctx context.Context,
	registry *project.Registry,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
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

	trend := newTokenTrendRecorder(defaultTokenTrendWindowSize)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := publishSnapshotOnce(ctx, registry, snapshotHub, now(), trend, lifetimeSource, dashboardURL); err != nil {
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

func publishStartupSnapshotOnce(
	ctx context.Context,
	cfg globalconfig.Config,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
	now time.Time,
) error {
	if snapshotHub == nil {
		return nil
	}
	snapshot := startupSnapshot(ctx, cfg, lifetimeSource, dashboardURL, now)
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
) telemetry.Snapshot {
	nextRefreshAt := now
	snapshot := telemetry.Snapshot{
		GeneratedAt:    now,
		Instance:       startupSnapshotInstance(cfg),
		Projects:       startupProjectSnapshots(cfg.Projects),
		DashboardURL:   cleanDashboardURL(dashboardURL),
		Shutdown:       telemetry.Shutdown{Status: "running"},
		Refresh:        telemetry.Refresh{NextRefreshAt: &nextRefreshAt},
		LifetimeTotals: lifetimeTotals(ctx, lifetimeSource),
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

func startupProjectSnapshots(projects []globalconfig.Project) []telemetry.ProjectSnapshot {
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
		})
	}
	return out
}

func publishSnapshotOnce(
	ctx context.Context,
	registry *project.Registry,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	now time.Time,
	trend *tokenTrendRecorder,
	lifetimeSource lifetimeTotalsSource,
	dashboardURL string,
) error {
	merged := telemetry.Snapshot{GeneratedAt: now}
	trackedProjects := registry.List()
	if len(trackedProjects) == 0 {
		return nil
	}
	for _, trackedProject := range trackedProjects {
		projectMetadata := projectSnapshotMetadata(trackedProject)
		if !trackedProject.Running() {
			nextRefreshAt := now
			merged = mergeSnapshot(merged, telemetry.Snapshot{
				Project:      projectMetadata,
				DashboardURL: cleanDashboardURL(dashboardURL),
				Shutdown:     telemetry.Shutdown{Status: "running"},
				Refresh:      telemetry.Refresh{NextRefreshAt: &nextRefreshAt},
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
			continue
		}
		snapshot := state.Snapshot(now)
		snapshot.Project = projectMetadata
		snapshot.DashboardURL = cleanDashboardURL(dashboardURL)
		merged = mergeSnapshot(merged, snapshot)
	}
	if trend != nil {
		merged = trend.apply(merged)
	}
	merged.LifetimeTotals = lifetimeTotals(ctx, lifetimeSource)
	if err := snapshotHub.Publish(merged); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	return nil
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
		Available:      true,
		InputTokens:    totals.InputTokens,
		OutputTokens:   totals.OutputTokens,
		TotalTokens:    totals.TotalTokens,
		RuntimeSeconds: totals.RuntimeSeconds,
		Sessions:       totals.Sessions,
		Runs:           totals.Runs,
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
	current.Queue = append(current.Queue, next.Queue...)
	current.Blocked = append(current.Blocked, next.Blocked...)
	current.Completed = append(current.Completed, next.Completed...)
	current.BoardIssues = append(current.BoardIssues, next.BoardIssues...)
	current.Pipeline = append(current.Pipeline, next.Pipeline...)
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

func stampSnapshotProjectID(snapshot telemetry.Snapshot) telemetry.Snapshot {
	projectID := strings.TrimSpace(snapshot.Project.ID)
	if projectID == "" {
		return snapshot
	}

	for i := range snapshot.Pipeline {
		snapshot.Pipeline[i] = stampIssueProjectID(snapshot.Pipeline[i], projectID)
	}
	for i := range snapshot.BoardIssues {
		snapshot.BoardIssues[i] = stampIssueProjectID(snapshot.BoardIssues[i], projectID)
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
	if current.PollIntervalSeconds == 0 ||
		(next.PollIntervalSeconds > 0 && next.PollIntervalSeconds < current.PollIntervalSeconds) {
		current.PollIntervalSeconds = next.PollIntervalSeconds
	}
	current.LastRefreshAt = latestTime(current.LastRefreshAt, next.LastRefreshAt)
	current.NextRefreshAt = earliestTime(current.NextRefreshAt, next.NextRefreshAt)
	return current
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
