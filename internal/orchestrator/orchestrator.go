package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultPollInterval             = 30 * time.Second
	defaultRunningReconcileInterval = 2 * time.Minute
	defaultWorkspaceCleanupIdleTTL  = 24 * time.Hour
	defaultWorkspaceCleanupSweep    = 10 * time.Minute
	gitHubGraphQLPauseRemaining     = 100
	gitHubGraphQLBackoffRemaining   = 500
	defaultMaxConcurrentAgents      = 1
	defaultMaxRetryBackoff          = 5 * time.Minute
	defaultContinuationRetry        = time.Second
	defaultFailureRetryBaseDelay    = 10 * time.Second
	continuationDispatchBackoff     = 100 * time.Millisecond
	runUpdateBufferSize             = 128
	blockedStatusState              = "Blocked"
	blockedReasonDependency         = "blocked by non-terminal dependency"
	blockedReasonProjectStatus      = "blocked by project status"
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
	Connector       connector.Connector
	Runner          Runner
	WorkspaceReaper WorkspaceReaper
	Logger          *slog.Logger
}

type WorkspaceReapResult = runpkg.WorkspaceReapResult

type WorkspaceReaper = runpkg.WorkspaceReaper

type RuntimeUpdate struct {
	Config    Config
	Connector connector.Connector
}

type Orchestrator struct {
	cfg           Config
	connector     connector.Connector
	supervisor    *runpkg.Supervisor
	reaper        WorkspaceReaper
	logger        *slog.Logger
	stateRequests chan stateRequest
	configUpdates chan configUpdateRequest
	refreshes     chan time.Time
	runResults    chan runpkg.Completion
	runUpdates    chan runUpdate
	done          chan struct{}
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
		cfg:           cfg,
		connector:     deps.Connector,
		supervisor:    supervisor,
		reaper:        reaper,
		logger:        logger,
		stateRequests: make(chan stateRequest),
		configUpdates: make(chan configUpdateRequest),
		refreshes:     make(chan time.Time, 1),
		runResults:    make(chan runpkg.Completion),
		runUpdates:    make(chan runUpdate, runUpdateBufferSize),
		done:          make(chan struct{}),
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

func (o *Orchestrator) tick(ctx context.Context, state *State, now time.Time) {
	o.markRefresh(state, now)
	defer o.finishRefresh(state, now)

	if pause := o.gitHubGraphQLPause(state, now); pause > 0 {
		o.logger.Warn("github graphql polling paused", "remaining", gitHubGraphQLRemaining(state), "pause", pause)
		return
	}

	o.reapWorkspacesIfDue(ctx, state, now)
	o.reconcileRunningIssues(ctx, state, now)
	o.heartbeatRunningClaims(ctx, state, now)

	issues, err := o.connector.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("fetch candidate issues failed", "error", err)
		return
	}
	statusIssues, statusErr := o.connector.FetchIssuesByStates(ctx, observedStatusFetchStates())
	if statusErr != nil {
		o.logger.Warn("fetch observed status issues failed", "error", statusErr)
	}

	issues = cloneIssues(issues)
	if statusErr == nil {
		statusIssues = cloneIssues(statusIssues)
		state.Pipeline = issuesInStates(statusIssues, prPipelineFetchStates())
	}
	observedIssues := cloneIssues(issues)
	if statusErr == nil {
		observedIssues = append(observedIssues, statusIssues...)
	}
	issues = filterCompletedEpicCandidates(issues, o.closeCompletedEpics(ctx, observedIssues))
	sortIssuesForDispatch(issues, o.cfg.DispatchPriorityByState)
	o.pruneBudgetRefusals(state, now)
	o.trackBlockedCandidates(state, issues, now)
	if statusErr == nil {
		o.trackBlockedStatusIssues(state, issuesInStates(statusIssues, []string{blockedStatusState}), now)
	}
	o.dispatchReadyIssues(ctx, state, issues, now)
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
		state.Running[id] = running
		if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
			cancelRunning(state, id)
		}

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
	o.captureConnectorRateLimits(state, now)

	interval := o.adaptivePollInterval(state, now)
	state.PollInterval = interval
	if interval > 0 {
		state.NextRefreshAt = now.Add(interval)
		return
	}
	state.NextRefreshAt = time.Time{}
}

func (o *Orchestrator) captureConnectorRateLimits(state *State, now time.Time) {
	reporter, ok := o.connector.(connector.RateLimitReporter)
	if !ok {
		return
	}
	rateLimit, ok := reporter.GraphQLRateLimit()
	if !ok {
		return
	}
	bucket := gitHubGraphQLBucket(rateLimit, now)
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	state.RateLimits.GitHubGraphQL = bucket
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
	seenBlocked := make(map[string]struct{})
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		if todoBlockedByNonTerminal(issue, o.cfg.TerminalStates) {
			seenBlocked[issue.ID] = struct{}{}
			state.Blocked[issue.ID] = Blocked{
				Issue:     cloneIssue(issue),
				Reason:    blockedReasonDependency,
				BlockedAt: now,
				Source:    BlockedSourceDependency,
			}
		}
	}

	for issueID, blocked := range state.Blocked {
		if !blockedFromDependency(blocked) {
			continue
		}
		if _, ok := seenBlocked[issueID]; !ok {
			delete(state.Blocked, issueID)
		}
	}
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
	dueRetries := dueRetriesByIssue(state, now)
	o.releaseMissingDueRetries(state, issues, dueRetries)

	continuations := 0
	for _, issue := range issues {
		if retry, ok := dueRetries[issue.ID]; ok {
			o.dispatchRetryIssue(ctx, state, issue, retry, now)
			continue
		}
		if availableSlots(state) == 0 {
			return
		}
		issue, ok := o.hydrateDispatchIssue(ctx, issue)
		if !ok {
			continue
		}
		if !o.dispatchable(issue, state, now) {
			if todoBlockedByNonTerminal(issue, o.cfg.TerminalStates) {
				state.Blocked[issue.ID] = Blocked{
					Issue:     cloneIssue(issue),
					Reason:    blockedReasonDependency,
					BlockedAt: now,
					Source:    BlockedSourceDependency,
				}
			}
			continue
		}

		delay := time.Duration(0)
		if continuationDispatch(issue) {
			delay = continuationDelay(continuations)
			continuations++
		}
		if !waitForDispatchBackoff(ctx, delay) {
			return
		}
		o.dispatchIssue(ctx, state, issue, 0, now, "")
	}
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

func (o *Orchestrator) releaseMissingDueRetries(
	state *State,
	issues []connector.Issue,
	dueRetries map[string]Retry,
) {
	if len(dueRetries) == 0 {
		return
	}

	byID := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = struct{}{}
	}

	for issueID := range dueRetries {
		if _, ok := byID[issueID]; !ok {
			if _, blocked := state.Blocked[issueID]; blocked {
				o.releaseClaim(state, issueID)
				continue
			}
			o.releaseIssue(state, issueID)
		}
	}
}

func (o *Orchestrator) dispatchRetryIssue(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	retry Retry,
	now time.Time,
) {
	delete(state.Retry, retry.Issue.ID)

	if !o.dispatchableForRetry(issue, state, now, retry.WorkerHost) {
		if o.budgetCooldownActive(state, issue.ID, now) {
			o.scheduleRetry(state, issue, retry.Attempt, now, "budget cooldown active", false, retry.WorkerHost)
			return
		}
		if !o.slotsAvailable(issue, state, retry.WorkerHost) {
			o.scheduleRetry(state, issue, retry.Attempt, now, "no available orchestrator slots", false, retry.WorkerHost)
			return
		}
		if _, blocked := state.Blocked[issue.ID]; blocked {
			o.releaseClaim(state, issue.ID)
			return
		}

		o.releaseIssue(state, issue.ID)
		return
	}

	if !o.dispatchIssue(ctx, state, issue, retry.Attempt, now, retry.WorkerHost) {
		o.scheduleRetry(state, issue, retry.Attempt, now, "claim verification failed", false, retry.WorkerHost)
	}
}

func (o *Orchestrator) dispatchable(issue connector.Issue, state *State, now time.Time) bool {
	return o.dispatchableIssue(issue, state, false, now, "")
}

func (o *Orchestrator) dispatchableForRetry(
	issue connector.Issue,
	state *State,
	now time.Time,
	preferredWorkerHost string,
) bool {
	return o.dispatchableIssue(issue, state, true, now, preferredWorkerHost)
}

func (o *Orchestrator) dispatchableIssue(
	issue connector.Issue,
	state *State,
	allowClaimed bool,
	now time.Time,
	preferredWorkerHost string,
) bool {
	if !validCandidate(issue) {
		return false
	}
	if !stateIn(issue.State, o.cfg.ActiveStates) || stateIn(issue.State, o.cfg.TerminalStates) {
		return false
	}
	if duplicatePullRequestWork(issue) {
		return false
	}
	if !o.authorized(issue) {
		return false
	}
	if todoBlockedByNonTerminal(issue, o.cfg.TerminalStates) {
		return false
	}
	if _, ok := state.Running[issue.ID]; ok {
		return false
	}
	if _, ok := state.Claimed[issue.ID]; ok && !allowClaimed {
		return false
	}
	if _, ok := state.Blocked[issue.ID]; ok {
		return false
	}
	if o.budgetCooldownActive(state, issue.ID, now) {
		return false
	}

	return o.slotsAvailable(issue, state, preferredWorkerHost)
}

func (o *Orchestrator) authorized(issue connector.Issue) bool {
	if !o.cfg.Authorization.Configured() {
		return true
	}
	return selector.Match(issue, o.cfg.Authorization, o.cfg.SelectorContext)
}

func (o *Orchestrator) slotsAvailable(issue connector.Issue, state *State, preferredWorkerHost string) bool {
	return availableSlots(state) > 0 &&
		o.stateSlotsAvailable(issue, state) &&
		o.workerSlotsAvailable(state, preferredWorkerHost)
}

func (o *Orchestrator) stateSlotsAvailable(issue connector.Issue, state *State) bool {
	limit := o.cfg.MaxConcurrentAgents
	if stateLimit, ok := o.cfg.MaxConcurrentAgentsByState[normalizeState(issue.State)]; ok {
		limit = stateLimit
	}

	used := 0
	normalized := normalizeState(issue.State)
	for _, running := range state.Running {
		if normalizeState(running.Issue.State) == normalized {
			used++
		}
	}

	return used < limit
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

	claimedIssue, claim, ok := o.claimIssue(ctx, issue, now)
	if !ok {
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
		cancel:     cancel,
	}
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
	if running.cancel != nil {
		running.cancel()
	}
	delete(state.Running, event.IssueID)

	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		delete(state.Claimed, event.IssueID)
		delete(state.Retry, event.IssueID)
		delete(state.BudgetRefusals, event.IssueID)
		o.reapWorkspace(context.Background(), state, running.Issue, workspaceReapReason(running.Issue, o.cfg.TerminalStates))
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
	if attempt < 1 {
		attempt = 1
	}

	o.scheduleRetryAfter(state, issue, attempt, now, o.retryDelay(attempt, continuation), err, workerHost)
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
	if attempt < 1 {
		attempt = 1
	}
	if delay < 0 {
		delay = 0
	}

	issue = cloneIssue(issue)
	state.Retry[issue.ID] = Retry{
		Issue:      issue,
		Attempt:    attempt,
		DueAt:      now.Add(delay),
		Error:      err,
		WorkerHost: workerHost,
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		state.Claimed[issue.ID] = Claimed{
			Issue:     issue,
			ClaimedAt: now,
		}
	}
}

func (o *Orchestrator) retryDelay(attempt int, continuation bool) time.Duration {
	if continuation {
		return o.cfg.ContinuationRetryDelay
	}
	if attempt < 1 {
		attempt = 1
	}
	exponent := attempt - 1
	if exponent > 30 {
		exponent = 30
	}

	delay := o.cfg.FailureRetryBaseDelay * time.Duration(math.Pow(2, float64(exponent)))
	if delay > o.cfg.MaxRetryBackoff {
		return o.cfg.MaxRetryBackoff
	}
	return delay
}

func (o *Orchestrator) releaseIssue(state *State, issueID string) {
	cancelRunning(state, issueID)
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Blocked, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
}

func (o *Orchestrator) releaseClaim(state *State, issueID string) {
	cancelRunning(state, issueID)
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
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
