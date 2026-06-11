package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultPollInterval               = 30 * time.Second
	defaultRunningReconcileInterval   = 2 * time.Minute
	defaultWorkspaceCleanupIdleTTL    = 24 * time.Hour
	defaultWorkspaceCleanupSweep      = 10 * time.Minute
	gitHubGraphQLPauseRemaining       = 100
	gitHubGraphQLBackoffRemaining     = 500
	defaultGitHubGraphQLWarnRemaining = 500
	defaultMaxConcurrentAgents        = 1
	defaultMaxRetryBackoff            = 5 * time.Minute
	defaultContinuationRetry          = time.Second
	defaultFailureRetryBaseDelay      = 10 * time.Second
	continuationDispatchBackoff       = 100 * time.Millisecond
	runUpdateBufferSize               = 128
	maxRecentEvents                   = 50
	blockedStatusState                = "Blocked"
	blockedReasonDependency           = "blocked by non-terminal dependency"
	blockedReasonProjectStatus        = "blocked by project status"
)

var prPipelineStates = []string{"Human Review", "Merging"}

var (
	ErrMissingConnector = errors.New("orchestrator connector is required")
	ErrStopped          = errors.New("orchestrator stopped")
)

type Config struct {
	PollInterval                  time.Duration
	MaxConcurrentAgents           int
	MaxConcurrentAgentsByState    map[string]int
	DispatchPriorityByState       []string
	MaxConcurrentAgentsPerHost    int
	MaxRetryBackoff               time.Duration
	Project                       scheduler.ProjectCandidate
	Claiming                      ClaimingConfig
	AutoPromote                   AutoPromoteConfig
	ActiveStates                  []string
	ObservedStates                []string
	TerminalStates                []string
	Authorization                 selector.Selector
	SelectorContext               selector.Context
	WorkerHosts                   []string
	BudgetRefusalCooldown         time.Duration
	WorkspaceCleanupIdleTTL       time.Duration
	WorkspaceCleanupSweepInterval time.Duration
	ContinuationRetryDelay        time.Duration
	FailureRetryBaseDelay         time.Duration
	SelectorPersona               string
	GitHubGraphQLWarnRemaining    int64
}

type ClaimingConfig struct {
	Enabled           bool
	OwnershipMode     string
	Owner             string
	AssigneeLogin     string
	OwnerField        string
	LeaseField        string
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
}

type Dependencies struct {
	Connector          connector.Connector
	Runner             Runner
	WorkspaceReaper    WorkspaceReaper
	GlobalDispatchGate scheduler.ProjectDispatchGate
	Logger             *slog.Logger
}

type WorkspaceReapResult = runpkg.WorkspaceReapResult

type WorkspaceReaper = runpkg.WorkspaceReaper

type RuntimeUpdate struct {
	Config    Config
	Connector connector.Connector
}

type Orchestrator struct {
	cfg                Config
	connector          connector.Connector
	supervisor         *runpkg.Supervisor
	reaper             WorkspaceReaper
	logger             *slog.Logger
	globalDispatchGate scheduler.ProjectDispatchGate
	stateRequests      chan stateRequest
	configUpdates      chan configUpdateRequest
	refreshes          chan time.Time
	runResults         chan runpkg.Completion
	runUpdates         chan runUpdate
	done               chan struct{}
}

type stateRequest struct {
	reply chan State
}

type configUpdateRequest struct {
	update RuntimeUpdate
	reply  chan struct{}
}

type runUpdate struct {
	issueID string
	usage   runpkg.UsageUpdate
}

func ConfigFromWorkflow(cfg workflowconfig.Config) Config {
	identity := cfg.Identity
	identity.Normalize()

	return Config{
		PollInterval:               durationFromMillis(cfg.Polling.IntervalMS),
		MaxConcurrentAgents:        cfg.Agent.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: cloneStateLimits(cfg.Agent.MaxConcurrentAgentsByState),
		DispatchPriorityByState:    append([]string(nil), cfg.Agent.DispatchPriorityByState...),
		MaxConcurrentAgentsPerHost: positiveIntValue(cfg.Worker.MaxConcurrentAgentsPerHost),
		MaxRetryBackoff:            durationFromMillis(cfg.Agent.MaxRetryBackoffMS),
		Claiming: ClaimingConfig{
			Enabled:           cfg.Tracker.Claims.Enabled,
			OwnershipMode:     identity.OwnershipMode,
			Owner:             identity.Name,
			AssigneeLogin:     identity.GitHubLogin,
			OwnerField:        identity.OwnerField,
			LeaseField:        cfg.Tracker.Claims.LeaseField,
			LeaseTTL:          durationFromSeconds(cfg.Tracker.Claims.TTLSeconds),
			HeartbeatInterval: durationFromSeconds(cfg.Tracker.Claims.HeartbeatSeconds),
		},
		AutoPromote: normalizeAutoPromoteConfig(AutoPromoteConfig{
			Enabled:            cfg.Agent.AutoPromote.Enabled,
			QuietDuration:      durationFromSeconds(cfg.Agent.AutoPromote.QuietSeconds),
			OptoutLabel:        cfg.Agent.AutoPromote.OptoutLabel,
			AllowedIssueLabels: append([]string(nil), cfg.Agent.AutoPromote.AllowedIssueLabels...),
			Gate:               gate.Effective(cfg.Gate),
		}),
		ActiveStates:                  append([]string(nil), cfg.Tracker.ActiveStates...),
		ObservedStates:                append([]string(nil), cfg.Tracker.ObservedStates...),
		TerminalStates:                append([]string(nil), cfg.Tracker.TerminalStates...),
		Authorization:                 cfg.Tracker.Authorization,
		SelectorContext:               selector.Context{InstanceLogin: identity.GitHubLogin, Persona: identity.Name},
		WorkerHosts:                   append([]string(nil), cfg.Worker.SSHHosts...),
		BudgetRefusalCooldown:         durationFromSeconds(cfg.Budget.RefusalCooldownSeconds),
		WorkspaceCleanupIdleTTL:       durationFromMillis(cfg.Workspace.CleanupIdleTTLMS),
		WorkspaceCleanupSweepInterval: durationFromMillis(cfg.Workspace.CleanupSweepIntervalMS),
		SelectorPersona:               cfg.Tracker.Assignee,
		GitHubGraphQLWarnRemaining:    int64(cfg.Tracker.GitHubGraphQLWarnRemaining),
	}
}

func New(cfg Config, deps Dependencies) (*Orchestrator, error) {
	cfg = normalizeConfig(cfg)
	if deps.Connector == nil {
		return nil, ErrMissingConnector
	}

	runner := deps.Runner
	if runner == nil {
		runner = FakeRunner{}
	}
	reaper := deps.WorkspaceReaper
	if reaper == nil {
		if candidate, ok := runner.(WorkspaceReaper); ok {
			reaper = candidate
		}
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	supervisor, err := runpkg.NewSupervisor(runner, runpkg.SupervisorConfig{
		MaxRetryBackoff:       cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: cfg.FailureRetryBaseDelay,
		Logger:                logger,
	})
	if err != nil {
		return nil, err
	}

	return &Orchestrator{
		cfg:                cfg,
		connector:          deps.Connector,
		supervisor:         supervisor,
		reaper:             reaper,
		logger:             logger,
		globalDispatchGate: deps.GlobalDispatchGate,
		stateRequests:      make(chan stateRequest),
		configUpdates:      make(chan configUpdateRequest),
		refreshes:          make(chan time.Time, 1),
		runResults:         make(chan runpkg.Completion),
		runUpdates:         make(chan runUpdate, runUpdateBufferSize),
		done:               make(chan struct{}),
	}, nil
}

func (o *Orchestrator) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	defer close(o.done)

	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()

	state := newState(o.cfg)
	defer o.releaseRunningSlots(&state)
	o.tick(ctx, &state, time.Now())
	resetTicker(ticker, state.PollInterval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			o.tick(ctx, &state, now)
			resetTicker(ticker, state.PollInterval)
		case now := <-o.refreshes:
			o.tick(ctx, &state, now)
			resetTicker(ticker, state.PollInterval)
		case result := <-o.runResults:
			o.handleRunResult(&state, result)
		case update := <-o.runUpdates:
			o.handleRunUpdate(&state, update)
		case update := <-o.configUpdates:
			o.applyRuntimeUpdate(&state, update.update, ticker)
			update.reply <- struct{}{}
		case request := <-o.stateRequests:
			request.reply <- state.clone()
		}
	}
}

func resetTicker(ticker *time.Ticker, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker.Reset(interval)
}

func (o *Orchestrator) UpdateConfig(ctx context.Context, cfg Config) error {
	return o.UpdateRuntime(ctx, RuntimeUpdate{Config: cfg})
}

func (o *Orchestrator) UpdateRuntime(ctx context.Context, update RuntimeUpdate) error {
	if ctx == nil {
		ctx = context.Background()
	}

	request := configUpdateRequest{
		update: update,
		reply:  make(chan struct{}, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case o.configUpdates <- request:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case <-request.reply:
		return nil
	}
}

func (o *Orchestrator) State(ctx context.Context) (State, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	request := stateRequest{reply: make(chan State, 1)}
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	case <-o.done:
		return State{}, ErrStopped
	case o.stateRequests <- request:
	}

	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	case <-o.done:
		return State{}, ErrStopped
	case state := <-request.reply:
		return state, nil
	}
}

func observedStatusFetchStates() []string {
	return append([]string{blockedStatusState}, prPipelineFetchStates()...)
}

func prPipelineFetchStates() []string {
	states := make([]string, 0, len(prPipelineStates))
	seen := make(map[string]struct{}, len(prPipelineStates))
	for _, state := range prPipelineStates {
		key := normalizeState(state)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		states = append(states, state)
	}
	return states
}

func issuesInStates(issues []connector.Issue, states []string) []connector.Issue {
	wanted := stateNameSet(states)
	if len(wanted) == 0 {
		return nil
	}

	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := wanted[normalizeState(issue.State)]; ok {
			out = append(out, cloneIssue(issue))
		}
	}
	return out
}

func blockedStatusTransitionIssues(blocked map[string]Blocked) []connector.Issue {
	out := make([]connector.Issue, 0, len(blocked))
	for _, entry := range blocked {
		legacyStatusIssue := entry.Source == "" && normalizeState(entry.Issue.State) == normalizeState(blockedStatusState)
		if entry.Source != BlockedSourceProjectStatus && !legacyStatusIssue {
			continue
		}
		if strings.TrimSpace(entry.Issue.ID) == "" {
			continue
		}
		out = append(out, cloneIssue(entry.Issue))
	}
	return out
}

func stateNameSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = normalizeState(state)
		if state != "" {
			out[state] = struct{}{}
		}
	}
	return out
}

func (o *Orchestrator) reconcileClosedCompletedIssueStatuses(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	targetState := doneStateName(o.cfg.TerminalStates)
	reconciled := map[string]struct{}{}
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if _, ok := reconciled[issueID]; ok {
			continue
		}
		if !closedCompletedIssueNeedsStatusReconciliation(issue, o.cfg.TerminalStates) {
			continue
		}
		if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
			if o.logger != nil {
				o.logger.Warn("reconcile closed completed issue status failed", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "error", err)
			}
			continue
		}
		reconciled[issueID] = struct{}{}
		if o.logger != nil {
			o.logger.Info("reconciled closed completed issue status", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState)
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "closed_completed_status_reconciled",
			Message: "reconciled " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState,
		})
	}
	if len(reconciled) == 0 {
		return nil
	}
	return reconciled
}

func closedCompletedIssueNeedsStatusReconciliation(issue connector.Issue, terminalStates []string) bool {
	return issue.Closed &&
		closedReasonCompleted(issue.ClosedReason) &&
		strings.TrimSpace(issue.State) != "" &&
		!stateIn(issue.State, terminalStates)
}

func closedReasonCompleted(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	reason = strings.ReplaceAll(reason, "-", "_")
	return reason == "completed"
}

func filterReconciledIssues(issues []connector.Issue, reconciled map[string]struct{}) []connector.Issue {
	if len(reconciled) == 0 || len(issues) == 0 {
		return issues
	}
	out := issues[:0]
	for _, issue := range issues {
		if _, ok := reconciled[strings.TrimSpace(issue.ID)]; ok {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func recordStateEvent(state *State, event telemetry.ActivityEvent) {
	if state == nil {
		return
	}
	state.RecentEvents = append(state.RecentEvents, event)
	if len(state.RecentEvents) > maxRecentEvents {
		state.RecentEvents = append([]telemetry.ActivityEvent(nil), state.RecentEvents[len(state.RecentEvents)-maxRecentEvents:]...)
	}
}

func issueLabel(issue connector.Issue) string {
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(issue.ID)
}

func (o *Orchestrator) reconcileRunningIssues(ctx context.Context, state *State, now time.Time) {
	ids := runningIssueIDs(state.Running)
	if len(ids) == 0 {
		return
	}
	if !o.shouldReconcileRunningIssues(state, now) {
		return
	}
	state.LastRunningReconcileAt = now

	issues, err := o.connector.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("fetch running issue states failed", "error", err)
		}
		return
	}

	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" {
			continue
		}
		byID[issue.ID] = issue
	}

	for _, id := range ids {
		issue, ok := byID[id]
		if !ok {
			continue
		}

		running := state.Running[id]
		running.Issue = mergeIssueTrackerFields(running.Issue, issue)
		if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
			o.completeTerminalRunning(ctx, state, id, running, terminalCompletedAt(running.Issue, now), running.Tokens)
			continue
		}
		state.Running[id] = running

		if claimed, ok := state.Claimed[id]; ok {
			claimed.Issue = mergeIssueTrackerFields(claimed.Issue, issue)
			state.Claimed[id] = claimed
		}
	}
}

func (o *Orchestrator) shouldReconcileRunningIssues(state *State, now time.Time) bool {
	if len(state.Running) == 0 {
		return false
	}
	if state.LastRunningReconcileAt.IsZero() {
		return true
	}
	interval := defaultRunningReconcileInterval
	if o.cfg.PollInterval > interval {
		interval = o.cfg.PollInterval
	}
	return !now.Before(state.LastRunningReconcileAt.Add(interval))
}

func mergeIssueTrackerFields(current, refreshed connector.Issue) connector.Issue {
	merged := cloneIssue(current)
	refreshed = cloneIssue(refreshed)

	if strings.TrimSpace(refreshed.ID) != "" {
		merged.ID = refreshed.ID
	}
	if refreshed.Identifier != "" {
		merged.Identifier = refreshed.Identifier
	}
	if refreshed.Title != "" {
		merged.Title = refreshed.Title
	}
	if refreshed.Description != "" {
		merged.Description = refreshed.Description
	}
	if refreshed.Priority != nil {
		merged.Priority = refreshed.Priority
	}
	if refreshed.State != "" {
		merged.State = refreshed.State
	}
	if refreshed.BranchName != "" {
		merged.BranchName = refreshed.BranchName
	}
	if refreshed.URL != "" {
		merged.URL = refreshed.URL
	}
	merged.Closed = refreshed.Closed
	if refreshed.PRNumber != nil {
		merged.PRNumber = refreshed.PRNumber
	}
	if refreshed.PullRequest != nil {
		merged.PullRequest = refreshed.PullRequest
	}
	if refreshed.AuthorID != "" {
		merged.AuthorID = refreshed.AuthorID
	}
	if refreshed.AssigneeID != "" {
		merged.AssigneeID = refreshed.AssigneeID
	}
	if refreshed.Assignees != nil {
		merged.Assignees = refreshed.Assignees
	}
	if refreshed.BlockedBy != nil {
		merged.BlockedBy = refreshed.BlockedBy
	}
	if refreshed.BlockerReason != "" {
		merged.BlockerReason = refreshed.BlockerReason
	}
	if refreshed.Labels != nil {
		merged.Labels = refreshed.Labels
	}
	if refreshed.Fields != nil {
		merged.Fields = refreshed.Fields
	}
	if refreshed.CreatedAt != nil {
		merged.CreatedAt = refreshed.CreatedAt
	}
	if refreshed.UpdatedAt != nil {
		merged.UpdatedAt = refreshed.UpdatedAt
	}
	if refreshed.StageUpdatedAt != nil {
		merged.StageUpdatedAt = refreshed.StageUpdatedAt
	}
	if refreshed.ModelOverride != "" {
		merged.ModelOverride = refreshed.ModelOverride
	}

	return merged
}

func runningIssueIDs(running map[string]Running) []string {
	ids := sortedKeys(running)
	out := ids[:0]
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

func (o *Orchestrator) markRefresh(state *State, now time.Time) {
	state.PollInterval = o.cfg.PollInterval
	state.MaxConcurrentAgents = o.cfg.MaxConcurrentAgents
	state.LastRefreshAt = now
	if o.cfg.PollInterval > 0 {
		state.NextRefreshAt = now.Add(o.cfg.PollInterval)
		return
	}
	state.NextRefreshAt = time.Time{}
}

func (o *Orchestrator) finishRefresh(state *State, now time.Time) {
	cycle := o.captureConnectorRateLimits(state, now)
	o.logGraphQLRateLimitCycle(cycle)

	interval := o.adaptivePollInterval(state, now)
	state.PollInterval = interval
	if interval > 0 {
		state.NextRefreshAt = now.Add(interval)
		return
	}
	state.NextRefreshAt = time.Time{}
}

type graphQLRateLimitCycle struct {
	Bucket     *telemetry.RateLimitBucket
	Cost       *telemetry.GraphQLCost
	HasSummary bool
}

func (o *Orchestrator) captureConnectorRateLimits(state *State, now time.Time) graphQLRateLimitCycle {
	var usage connector.GraphQLRateLimitUsage
	if reporter, ok := o.connector.(connector.GraphQLRateLimitUsageReporter); ok {
		usage = reporter.FlushGraphQLRateLimitUsage()
	}

	var rateLimit connector.GraphQLRateLimit
	hasRateLimit := usage.HasRateLimit
	if hasRateLimit {
		rateLimit = usage.RateLimit
	} else {
		reporter, ok := o.connector.(connector.RateLimitReporter)
		if !ok {
			return graphQLRateLimitCycle{}
		}
		var okRateLimit bool
		rateLimit, okRateLimit = reporter.GraphQLRateLimit()
		if !okRateLimit {
			return graphQLRateLimitCycle{}
		}
		hasRateLimit = true
	}

	cost := graphQLCostSummary(usage)
	bucket := gitHubGraphQLBucket(rateLimit, now)
	if cost != nil {
		bucket.Cost = cost.TotalCost
	}
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	state.RateLimits.GitHubGraphQL = bucket
	state.RateLimits.GraphQLCost = cost
	return graphQLRateLimitCycle{
		Bucket:     bucket,
		Cost:       cost,
		HasSummary: hasRateLimit,
	}
}

func graphQLCostSummary(usage connector.GraphQLRateLimitUsage) *telemetry.GraphQLCost {
	if usage.TotalQueries == 0 && usage.TotalCost == 0 && len(usage.QueryCosts) == 0 {
		return nil
	}

	cost := &telemetry.GraphQLCost{
		TotalQueries: usage.TotalQueries,
		TotalCost:    usage.TotalCost,
		Contributors: make([]telemetry.GraphQLCostContributor, 0, len(usage.QueryCosts)),
	}
	for _, contributor := range usage.QueryCosts {
		cost.Contributors = append(cost.Contributors, telemetry.GraphQLCostContributor{
			QueryType: contributor.QueryType,
			Count:     contributor.Count,
			Cost:      contributor.Cost,
		})
	}
	return cost
}

func (o *Orchestrator) logGraphQLRateLimitCycle(cycle graphQLRateLimitCycle) {
	if !cycle.HasSummary || cycle.Bucket == nil {
		return
	}

	var resetAt time.Time
	if cycle.Bucket.ResetAt != nil {
		resetAt = *cycle.Bucket.ResetAt
	}
	cycleCost := int64(0)
	queryCount := int64(0)
	var contributors []telemetry.GraphQLCostContributor
	if cycle.Cost != nil {
		cycleCost = cycle.Cost.TotalCost
		queryCount = cycle.Cost.TotalQueries
		contributors = cycle.Cost.Contributors
	}

	o.logger.Info(
		"github graphql budget summary",
		"cycle_cost", cycleCost,
		"query_count", queryCount,
		"remaining", cycle.Bucket.Remaining,
		"limit", cycle.Bucket.Limit,
		"reset_at", resetAt,
		"contributors", contributors,
	)

	if cycle.Bucket.Remaining < o.cfg.GitHubGraphQLWarnRemaining {
		o.logger.Warn(
			"github graphql budget below warning floor",
			"remaining", cycle.Bucket.Remaining,
			"warning_floor", o.cfg.GitHubGraphQLWarnRemaining,
			"limit", cycle.Bucket.Limit,
			"reset_at", resetAt,
		)
	}
}

func gitHubGraphQLBucket(rateLimit connector.GraphQLRateLimit, now time.Time) *telemetry.RateLimitBucket {
	var resetAt *time.Time
	var resetInSeconds int64
	if !rateLimit.ResetAt.IsZero() {
		value := rateLimit.ResetAt
		resetAt = &value
	}
	if rateLimit.RetryAfter > 0 {
		updatedAt := rateLimit.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = now
		}
		value := updatedAt.Add(rateLimit.RetryAfter)
		resetAt = &value
		resetInSeconds = int64(rateLimit.RetryAfter.Round(time.Second) / time.Second)
	}

	return &telemetry.RateLimitBucket{
		Remaining:      rateLimit.Remaining,
		Used:           rateLimit.Used,
		Limit:          rateLimit.Limit,
		Cost:           rateLimit.Cost,
		ResetAt:        resetAt,
		ResetInSeconds: resetInSeconds,
	}
}

func (o *Orchestrator) adaptivePollInterval(state *State, now time.Time) time.Duration {
	base := o.cfg.PollInterval
	if base <= 0 {
		base = defaultPollInterval
	}

	if pause := o.gitHubGraphQLPause(state, now); pause > base {
		return pause
	}

	bucket := gitHubGraphQLBucketFromState(state)
	if bucket == nil || bucket.Remaining <= 0 || bucket.Remaining >= gitHubGraphQLBackoffRemaining {
		return base
	}

	multiplier := int64(gitHubGraphQLBackoffRemaining) / bucket.Remaining
	if int64(gitHubGraphQLBackoffRemaining)%bucket.Remaining != 0 {
		multiplier++
	}
	if multiplier < 2 {
		multiplier = 2
	}
	return base * time.Duration(multiplier)
}

func (o *Orchestrator) gitHubGraphQLPause(state *State, now time.Time) time.Duration {
	bucket := gitHubGraphQLBucketFromState(state)
	if bucket == nil || bucket.ResetAt == nil {
		return 0
	}
	if bucket.ResetInSeconds > 0 && bucket.ResetAt.After(now) {
		return bucket.ResetAt.Sub(now)
	}
	if bucket.Remaining >= gitHubGraphQLPauseRemaining {
		return 0
	}
	if !bucket.ResetAt.After(now) {
		return 0
	}
	return bucket.ResetAt.Sub(now)
}

func gitHubGraphQLBucketFromState(state *State) *telemetry.RateLimitBucket {
	if state.RateLimits == nil {
		return nil
	}
	return state.RateLimits.GitHubGraphQL
}

func gitHubGraphQLRemaining(state *State) int64 {
	bucket := gitHubGraphQLBucketFromState(state)
	if bucket == nil {
		return 0
	}
	return bucket.Remaining
}

func (o *Orchestrator) applyRuntimeUpdate(state *State, update RuntimeUpdate, ticker *time.Ticker) {
	cfg := normalizeConfig(update.Config)
	o.cfg = cfg
	if update.Connector != nil {
		o.connector = update.Connector
	}
	o.supervisor.UpdateConfig(runpkg.SupervisorConfig{
		MaxRetryBackoff:       cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: cfg.FailureRetryBaseDelay,
	})
	state.PollInterval = cfg.PollInterval
	state.MaxConcurrentAgents = cfg.MaxConcurrentAgents
	state.Instance = instanceSnapshot(cfg)
	if !state.LastRefreshAt.IsZero() && cfg.PollInterval > 0 {
		state.NextRefreshAt = state.LastRefreshAt.Add(cfg.PollInterval)
	}
	ticker.Reset(cfg.PollInterval)
}

func (o *Orchestrator) trackBlockedCandidates(state *State, issues []connector.Issue, now time.Time) {
	o.dispatchPlanner().trackBlockedCandidates(state, issues, now)
}

func (o *Orchestrator) trackBlockedStatusIssues(state *State, issues []connector.Issue, now time.Time) {
	seenBlocked := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		seenBlocked[issue.ID] = struct{}{}
		state.Blocked[issue.ID] = Blocked{
			Issue:     cloneIssue(issue),
			Reason:    blockedStatusReason(issue),
			BlockedAt: now,
			Source:    BlockedSourceProjectStatus,
		}
	}

	for issueID, blocked := range state.Blocked {
		if blocked.Source != BlockedSourceProjectStatus {
			continue
		}
		if _, ok := seenBlocked[issueID]; !ok {
			delete(state.Blocked, issueID)
		}
	}
}

func blockedFromDependency(blocked Blocked) bool {
	return blocked.Source == BlockedSourceDependency ||
		(blocked.Source == "" && blocked.Reason == blockedReasonDependency)
}

func blockedStatusReason(issue connector.Issue) string {
	reason := strings.TrimSpace(issue.BlockerReason)
	if reason != "" {
		return reason
	}
	return blockedReasonProjectStatus
}

func (o *Orchestrator) dispatchReadyIssues(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	o.markGlobalProjectIdle()
	planner := o.dispatchPlanner()
	planner.plan(state, issues, now, dispatchPlanHooks{
		hydrate: func(issue connector.Issue) (connector.Issue, bool) {
			return o.hydrateDispatchIssue(ctx, issue)
		},
		beforeDispatch: func(_ connector.Issue, continuationIndex int) bool {
			if continuationIndex < 0 {
				return true
			}
			return waitForDispatchBackoff(ctx, continuationDelay(continuationIndex))
		},
		dispatch: func(issue connector.Issue, attempt int, workerHost string) bool {
			return o.dispatchIssue(ctx, state, issue, attempt, now, workerHost)
		},
		retryDispatchFailed: func(issue connector.Issue, retry Retry) {
			planner.scheduleRetry(state, issue, retry.Attempt, now, "claim verification failed", false, retry.WorkerHost)
		},
	})
}

func (o *Orchestrator) hydrateDispatchIssue(ctx context.Context, issue connector.Issue) (connector.Issue, bool) {
	if strings.TrimSpace(issue.ID) == "" || len(issue.Fields) > 0 || o.connector == nil {
		return issue, true
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("hydrate dispatch issue failed", "issue_id", issue.ID, "error", err)
		}
		return connector.Issue{}, false
	}
	for _, hydrated := range issues {
		if hydrated.ID == issue.ID {
			return mergeIssueTrackerFields(issue, hydrated), true
		}
	}
	return connector.Issue{}, false
}

func (o *Orchestrator) dispatchCandidates(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	o.markGlobalProjectIdle()
	for _, issue := range issues {
		if availableSlots(state) == 0 {
			return
		}
		issue, ok := o.hydrateDispatchIssue(ctx, issue)
		if !ok {
			continue
		}
		if !o.dispatchable(issue, state, now) {
			continue
		}

		o.dispatchIssue(ctx, state, issue, 0, now, "")
	}
}

func dueRetriesByIssue(state *State, now time.Time) map[string]Retry {
	retries := make(map[string]Retry, len(state.Retry))
	for _, retry := range state.Retry {
		if !retry.DueAt.After(now) {
			retries[retry.Issue.ID] = retry
		}
	}
	return retries
}

func (o *Orchestrator) dispatchable(issue connector.Issue, state *State, now time.Time) bool {
	return o.dispatchPlanner().dispatchable(issue, state, now)
}

func (o *Orchestrator) dispatchIssue(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	preferredWorkerHost string,
) bool {
	workerHost, ok := o.selectWorkerHost(state, preferredWorkerHost)
	if !ok {
		return false
	}

	globalSlot, ok := o.acquireGlobalDispatchSlot(ctx, issue, workerHost, now)
	if !ok {
		return false
	}

	claimedIssue, claim, ok := o.claimIssue(ctx, issue, now)
	if !ok {
		o.releaseGlobalDispatchSlot(globalSlot)
		return false
	}

	issue = cloneIssue(claimedIssue)
	claim.Issue = issue
	runCtx, cancel := context.WithCancel(ctx)
	state.Running[issue.ID] = Running{
		Issue:      issue,
		Attempt:    attempt,
		StartedAt:  now,
		WorkerHost: workerHost,
		globalSlot: globalSlot,
		cancel:     cancel,
	}
	o.setGlobalDispatchPreempt(globalSlot, cancel)
	state.Claimed[issue.ID] = claim
	delete(state.Retry, issue.ID)
	delete(state.Blocked, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.ReapedWorkspaces, issue.ID)

	request := RunRequest{
		Issue:           issue,
		Attempt:         attempt,
		StartedAt:       now,
		WorkerHost:      workerHost,
		SelectorContext: o.selectorContext(),
		OnUsageUpdate:   o.usageUpdateHandler(runCtx, issue.ID),
	}
	o.supervisor.Dispatch(runCtx, request, o.runResults)
	return true
}

func (o *Orchestrator) markGlobalProjectIdle() {
	if o.globalDispatchGate == nil {
		return
	}
	o.globalDispatchGate.MarkIdle(o.cfg.Project.ID)
}

func (o *Orchestrator) acquireGlobalDispatchSlot(
	ctx context.Context,
	issue connector.Issue,
	workerHost string,
	now time.Time,
) (scheduler.Slot, bool) {
	if o.globalDispatchGate == nil {
		return scheduler.Slot{}, true
	}

	slot, ok, err := o.globalDispatchGate.TryAcquire(ctx, o.cfg.Project, scheduler.SlotRequest{
		State: issue.State,
		Host:  workerHost,
	}, now)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("global dispatch slot unavailable", "project_id", o.cfg.Project.ID, "issue_id", issue.ID, "error", err)
		}
		return scheduler.Slot{}, false
	}
	return slot, ok
}

func (o *Orchestrator) releaseGlobalDispatchSlot(slot scheduler.Slot) {
	if o.globalDispatchGate == nil || slot == (scheduler.Slot{}) {
		return
	}
	if err := o.globalDispatchGate.Release(slot); err != nil && o.logger != nil {
		o.logger.Warn("release global dispatch slot failed", "project_id", o.cfg.Project.ID, "error", err)
	}
}

func (o *Orchestrator) setGlobalDispatchPreempt(slot scheduler.Slot, preempt func()) {
	if o.globalDispatchGate == nil || slot == (scheduler.Slot{}) {
		return
	}
	o.globalDispatchGate.SetPreempt(slot, preempt)
}

func (o *Orchestrator) selectorContext() selector.Context {
	ctx := selector.Context{
		Persona: o.cfg.SelectorPersona,
	}
	if identifier, ok := o.connector.(connector.InstanceIdentifier); ok {
		ctx.InstanceLogin = identifier.InstanceLogin()
	}
	return ctx
}

func (o *Orchestrator) usageUpdateHandler(ctx context.Context, issueID string) runpkg.UsageUpdateHandler {
	return func(update runpkg.UsageUpdate) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		select {
		case o.runUpdates <- runUpdate{issueID: issueID, usage: update}:
			return nil
		default:
			return nil
		}
	}
}

func (o *Orchestrator) handleRunUpdate(state *State, event runUpdate) {
	running, ok := state.Running[event.issueID]
	if !ok {
		return
	}

	if event.usage.SessionID != "" {
		running.SessionID = event.usage.SessionID
	}
	if event.usage.TurnCount > 0 {
		running.TurnCount = event.usage.TurnCount
	}
	if !event.usage.LastEventAt.IsZero() {
		running.LastEventAt = event.usage.LastEventAt
	}
	if event.usage.LastEvent != "" {
		running.LastEvent = event.usage.LastEvent
	}
	if event.usage.LastMessage != "" {
		running.LastMessage = event.usage.LastMessage
	}
	if len(event.usage.RecentEvents) > 0 {
		running.RecentEvents = cloneActivityEvents(event.usage.RecentEvents)
	}
	if event.usage.ProcessIdentity != "" {
		running.ProcessIdentity = event.usage.ProcessIdentity
	}
	if diffStatsPresent(event.usage.DiffStats) {
		running.DiffStats = event.usage.DiffStats
	}
	running.Tokens = event.usage.Tokens
	state.Running[event.issueID] = running
	if event.usage.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.usage.RateLimits)
	}
}

func (o *Orchestrator) handleRunResult(state *State, event runpkg.Completion) {
	running, ok := state.Running[event.IssueID]
	if !ok {
		return
	}
	o.releaseGlobalDispatchSlot(running.globalSlot)
	running.globalSlot = scheduler.Slot{}
	if running.cancel != nil {
		running.cancel()
	}
	delete(state.Running, event.IssueID)

	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		tokens := event.Result.Tokens
		if tokens == (CodexTotals{}) {
			tokens = running.Tokens
		}
		if diffStatsPresent(event.Result.DiffStats) {
			running.DiffStats = event.Result.DiffStats
		}
		o.completeTerminalRunning(context.Background(), state, event.IssueID, running, terminalCompletedAt(running.Issue, event.CompletedAt), tokens)
		if event.Result.RateLimits != nil {
			state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
		}
		return
	}

	if event.Err != nil {
		attempt := event.RetryAttempt
		if attempt < 1 {
			attempt = nextAttempt(running.Attempt)
		}
		delay := event.RetryDelay
		if delay <= 0 {
			delay = o.retryDelay(attempt, false)
		}
		o.scheduleRetryAfter(
			state,
			running.Issue,
			attempt,
			event.CompletedAt,
			delay,
			event.Err.Error(),
			running.WorkerHost,
		)
		return
	}

	finalState := event.Result.FinalState
	if finalState == "" {
		finalState = FinalStateCompleted
	}

	state.Completed[event.IssueID] = Completed{
		Issue:       cloneIssue(running.Issue),
		StartedAt:   running.StartedAt,
		CompletedAt: event.CompletedAt,
		FinalState:  finalState,
		Tokens:      event.Result.Tokens,
	}
	state.CodexTotals = addCodexTotals(state.CodexTotals, event.Result.Tokens)
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	if diffStatsPresent(event.Result.DiffStats) {
		state.DiffStats[event.IssueID] = event.Result.DiffStats
	}
	if event.Result.BudgetRefusal != nil {
		refusal := *event.Result.BudgetRefusal
		refusal.Issue = cloneIssue(running.Issue)
		state.BudgetRefusals[event.IssueID] = refusal
	}

	o.scheduleRetry(state, running.Issue, 1, event.CompletedAt, "", true, running.WorkerHost)
}

func (o *Orchestrator) scheduleRetry(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	err string,
	continuation bool,
	workerHost string,
) {
	o.dispatchPlanner().scheduleRetry(state, issue, attempt, now, err, continuation, workerHost)
}

func (o *Orchestrator) scheduleRetryAfter(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	delay time.Duration,
	err string,
	workerHost string,
) {
	o.dispatchPlanner().scheduleRetryAfter(state, issue, attempt, now, delay, err, workerHost)
}

func (o *Orchestrator) retryDelay(attempt int, continuation bool) time.Duration {
	return o.dispatchPlanner().retryDelay(attempt, continuation)
}

func (o *Orchestrator) releaseClaim(state *State, issueID string) {
	o.cancelRunning(state, issueID)
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
}

func (o *Orchestrator) completeTerminalRunning(
	ctx context.Context,
	state *State,
	issueID string,
	running Running,
	completedAt time.Time,
	tokens CodexTotals,
) {
	o.releaseGlobalDispatchSlot(running.globalSlot)
	if running.cancel != nil {
		running.cancel()
	}
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	finalState := strings.TrimSpace(running.Issue.State)
	if finalState == "" {
		finalState = FinalStateCompleted
	}
	state.Completed[issueID] = Completed{
		Issue:       cloneIssue(running.Issue),
		StartedAt:   running.StartedAt,
		CompletedAt: completedAt,
		FinalState:  finalState,
		Tokens:      tokens,
	}
	state.CodexTotals = addCodexTotals(state.CodexTotals, tokens)
	if diffStatsPresent(running.DiffStats) {
		state.DiffStats[issueID] = running.DiffStats
	}
	o.reapWorkspace(ctx, state, running.Issue, workspaceReapReason(running.Issue, o.cfg.TerminalStates))
}

func terminalCompletedAt(issue connector.Issue, fallback time.Time) time.Time {
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return *issue.StageUpdatedAt
	}
	if issue.UpdatedAt != nil && !issue.UpdatedAt.IsZero() {
		return *issue.UpdatedAt
	}
	if !fallback.IsZero() {
		return fallback
	}
	return time.Now().UTC()
}

func (o *Orchestrator) cancelRunning(state *State, issueID string) {
	running, ok := state.Running[issueID]
	if !ok {
		return
	}
	o.releaseGlobalDispatchSlot(running.globalSlot)
	running.globalSlot = scheduler.Slot{}
	state.Running[issueID] = running
	cancelRunning(state, issueID)
}

func cancelRunning(state *State, issueID string) {
	running, ok := state.Running[issueID]
	if !ok || running.cancel == nil {
		return
	}
	running.cancel()
	running.cancel = nil
	state.Running[issueID] = running
}

func (o *Orchestrator) releaseRunningSlots(state *State) {
	for issueID, running := range state.Running {
		o.releaseGlobalDispatchSlot(running.globalSlot)
		running.globalSlot = scheduler.Slot{}
		state.Running[issueID] = running
	}
}

func normalizeConfig(cfg Config) Config {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.MaxConcurrentAgents <= 0 {
		cfg.MaxConcurrentAgents = defaultMaxConcurrentAgents
	}
	if cfg.MaxRetryBackoff <= 0 {
		cfg.MaxRetryBackoff = defaultMaxRetryBackoff
	}
	if cfg.ContinuationRetryDelay < 0 {
		cfg.ContinuationRetryDelay = 0
	}
	if cfg.ContinuationRetryDelay == 0 {
		cfg.ContinuationRetryDelay = defaultContinuationRetry
	}
	if cfg.FailureRetryBaseDelay <= 0 {
		cfg.FailureRetryBaseDelay = defaultFailureRetryBaseDelay
	}
	if cfg.GitHubGraphQLWarnRemaining <= 0 {
		cfg.GitHubGraphQLWarnRemaining = defaultGitHubGraphQLWarnRemaining
	}
	if len(cfg.ActiveStates) == 0 {
		cfg.ActiveStates = []string{"Todo", "In Progress"}
	}
	if len(cfg.TerminalStates) == 0 {
		cfg.TerminalStates = []string{"Done", "Cancelled", "Canceled", "Closed"}
	}
	if cfg.WorkspaceCleanupIdleTTL <= 0 {
		cfg.WorkspaceCleanupIdleTTL = defaultWorkspaceCleanupIdleTTL
	}
	if cfg.WorkspaceCleanupSweepInterval <= 0 {
		cfg.WorkspaceCleanupSweepInterval = defaultWorkspaceCleanupSweep
	}

	cfg.ActiveStates = normalizedStates(cfg.ActiveStates)
	cfg.ObservedStates = normalizedStates(cfg.ObservedStates)
	cfg.TerminalStates = normalizedStates(cfg.TerminalStates)
	cfg.MaxConcurrentAgentsByState = cloneStateLimits(cfg.MaxConcurrentAgentsByState)
	cfg.DispatchPriorityByState = normalizedStates(cfg.DispatchPriorityByState)
	cfg.Claiming = normalizeClaimingConfig(cfg.Claiming)
	cfg.AutoPromote = normalizeAutoPromoteConfig(cfg.AutoPromote)
	cfg.Authorization.Normalize()
	cfg.SelectorContext.InstanceLogin = strings.TrimSpace(cfg.SelectorContext.InstanceLogin)
	cfg.SelectorContext.Persona = strings.TrimSpace(cfg.SelectorContext.Persona)
	cfg.WorkerHosts = normalizeWorkerHosts(cfg.WorkerHosts)
	cfg.SelectorPersona = strings.TrimSpace(cfg.SelectorPersona)
	if cfg.MaxConcurrentAgentsPerHost < 0 {
		cfg.MaxConcurrentAgentsPerHost = 0
	}

	return cfg
}

func normalizeClaimingConfig(cfg ClaimingConfig) ClaimingConfig {
	cfg.OwnershipMode = strings.ToLower(strings.TrimSpace(cfg.OwnershipMode))
	if cfg.OwnershipMode == "" {
		cfg.OwnershipMode = workflowconfig.IdentityOwnershipAssignee
	}
	cfg.Owner = strings.TrimSpace(cfg.Owner)
	cfg.AssigneeLogin = strings.TrimSpace(cfg.AssigneeLogin)
	cfg.OwnerField = strings.TrimSpace(cfg.OwnerField)
	cfg.LeaseField = strings.TrimSpace(cfg.LeaseField)
	if cfg.LeaseTTL < 0 {
		cfg.LeaseTTL = 0
	}
	if cfg.HeartbeatInterval < 0 {
		cfg.HeartbeatInterval = 0
	}
	return cfg
}

func durationFromMillis(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func durationFromSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func positiveIntValue(value *int) int {
	if value == nil || *value <= 0 {
		return 0
	}
	return *value
}

func cloneStateLimits(limits map[string]int) map[string]int {
	cloned := make(map[string]int, len(limits))
	for state, limit := range limits {
		if limit > 0 {
			cloned[normalizeState(state)] = limit
		}
	}
	return cloned
}

func cloneIssues(issues []connector.Issue) []connector.Issue {
	cloned := make([]connector.Issue, len(issues))
	for i, issue := range issues {
		cloned[i] = cloneIssue(issue)
	}
	return cloned
}

func validCandidate(issue connector.Issue) bool {
	return issue.ID != "" &&
		issue.Identifier != "" &&
		issue.Title != "" &&
		issue.State != "" &&
		!issue.Closed &&
		issue.AssignedToWorker
}

func duplicatePullRequestWork(issue connector.Issue) bool {
	if issue.PullRequest == nil {
		return false
	}
	switch normalizePullRequestState(issue.PullRequest.State) {
	case "merged":
		return true
	case "open":
		return normalizeState(issue.State) == "todo"
	default:
		return false
	}
}

func continuationDispatch(issue connector.Issue) bool {
	state := normalizeState(issue.State)
	return state != "" && state != "todo"
}

func continuationDelay(index int) time.Duration {
	if index <= 0 {
		return 0
	}
	return continuationDispatchBackoff
}

func waitForDispatchBackoff(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func normalizePullRequestState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func todoBlockedByNonTerminal(issue connector.Issue, terminalStates []string) bool {
	if normalizeState(issue.State) != "todo" {
		return false
	}

	for _, blocker := range issue.BlockedBy {
		if strings.TrimSpace(blocker.State) == "" {
			continue
		}
		if !stateIn(blocker.State, terminalStates) {
			return true
		}
	}
	return false
}

func availableSlots(state *State) int {
	available := state.MaxConcurrentAgents - len(state.Running)
	if available < 0 {
		return 0
	}
	return available
}

func nextAttempt(attempt int) int {
	if attempt < 1 {
		return 1
	}
	return attempt + 1
}

func stateIn(state string, states []string) bool {
	normalized := normalizeState(state)
	for _, candidate := range states {
		if normalized == candidate {
			return true
		}
	}
	return false
}

func normalizedStates(states []string) []string {
	normalized := make([]string, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = normalizeState(state)
		if state == "" {
			continue
		}
		if _, ok := seen[state]; ok {
			continue
		}
		seen[state] = struct{}{}
		normalized = append(normalized, state)
	}
	return normalized
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}
