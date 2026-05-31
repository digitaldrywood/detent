package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony-go/internal/config"
	"github.com/digitaldrywood/symphony-go/internal/connector"
)

const (
	defaultPollInterval          = 30 * time.Second
	defaultMaxConcurrentAgents   = 1
	defaultMaxRetryBackoff       = 5 * time.Minute
	defaultContinuationRetry     = time.Second
	defaultFailureRetryBaseDelay = 10 * time.Second
)

var (
	ErrMissingConnector = errors.New("orchestrator connector is required")
	ErrStopped          = errors.New("orchestrator stopped")
)

type Config struct {
	PollInterval               time.Duration
	MaxConcurrentAgents        int
	MaxConcurrentAgentsByState map[string]int
	DispatchPriorityByState    []string
	MaxConcurrentAgentsPerHost int
	MaxRetryBackoff            time.Duration
	AutoPromote                AutoPromoteConfig
	ActiveStates               []string
	TerminalStates             []string
	WorkerHosts                []string
	BudgetRefusalCooldown      time.Duration
	ContinuationRetryDelay     time.Duration
	FailureRetryBaseDelay      time.Duration
}

type Dependencies struct {
	Connector connector.Connector
	Runner    Runner
	Logger    *slog.Logger
}

type Orchestrator struct {
	cfg           Config
	connector     connector.Connector
	runner        Runner
	logger        *slog.Logger
	stateRequests chan stateRequest
	runResults    chan runResultEvent
	done          chan struct{}
}

type stateRequest struct {
	reply chan State
}

type runResultEvent struct {
	issueID     string
	result      RunResult
	err         error
	completedAt time.Time
}

func ConfigFromWorkflow(cfg workflowconfig.Config) Config {
	return Config{
		PollInterval:               durationFromMillis(cfg.Polling.IntervalMS),
		MaxConcurrentAgents:        cfg.Agent.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: cloneStateLimits(cfg.Agent.MaxConcurrentAgentsByState),
		DispatchPriorityByState:    append([]string(nil), cfg.Agent.DispatchPriorityByState...),
		MaxConcurrentAgentsPerHost: positiveIntValue(cfg.Worker.MaxConcurrentAgentsPerHost),
		MaxRetryBackoff:            durationFromMillis(cfg.Agent.MaxRetryBackoffMS),
		AutoPromote: normalizeAutoPromoteConfig(AutoPromoteConfig{
			Enabled:            cfg.Agent.AutoPromote.Enabled,
			QuietDuration:      durationFromSeconds(cfg.Agent.AutoPromote.QuietSeconds),
			OptoutLabel:        cfg.Agent.AutoPromote.OptoutLabel,
			AllowedIssueLabels: append([]string(nil), cfg.Agent.AutoPromote.AllowedIssueLabels...),
		}),
		ActiveStates:          append([]string(nil), cfg.Tracker.ActiveStates...),
		TerminalStates:        append([]string(nil), cfg.Tracker.TerminalStates...),
		WorkerHosts:           append([]string(nil), cfg.Worker.SSHHosts...),
		BudgetRefusalCooldown: durationFromSeconds(cfg.Budget.RefusalCooldownSeconds),
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

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Orchestrator{
		cfg:           cfg,
		connector:     deps.Connector,
		runner:        runner,
		logger:        logger,
		stateRequests: make(chan stateRequest),
		runResults:    make(chan runResultEvent),
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			o.tick(ctx, &state, now)
		case result := <-o.runResults:
			o.handleRunResult(&state, result)
		case request := <-o.stateRequests:
			request.reply <- state.clone()
		}
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
	issues, err := o.connector.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("fetch candidate issues failed", "error", err)
		return
	}

	issues = cloneIssues(issues)
	sortIssuesForDispatch(issues, o.cfg.DispatchPriorityByState)
	o.pruneBudgetRefusals(state, now)
	o.trackBlockedCandidates(state, issues, now)
	o.dispatchReadyIssues(ctx, state, issues, now)
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
				Reason:    "blocked by non-terminal dependency",
				BlockedAt: now,
			}
		}
	}

	for issueID, blocked := range state.Blocked {
		if blocked.Reason != "blocked by non-terminal dependency" {
			continue
		}
		if _, ok := seenBlocked[issueID]; !ok {
			delete(state.Blocked, issueID)
		}
	}
}

func (o *Orchestrator) dispatchReadyIssues(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	dueRetries := dueRetriesByIssue(state, now)
	o.releaseMissingDueRetries(state, issues, dueRetries)

	for _, issue := range issues {
		if retry, ok := dueRetries[issue.ID]; ok {
			o.dispatchRetryIssue(ctx, state, issue, retry, now)
			continue
		}
		if availableSlots(state) == 0 {
			return
		}
		if !o.dispatchable(issue, state, now) {
			continue
		}

		o.dispatchIssue(ctx, state, issue, 0, now, "")
	}
}

func (o *Orchestrator) dispatchCandidates(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	for _, issue := range issues {
		if availableSlots(state) == 0 {
			return
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

	o.dispatchIssue(ctx, state, issue, retry.Attempt, now, retry.WorkerHost)
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
) {
	workerHost, ok := o.selectWorkerHost(state, preferredWorkerHost)
	if !ok {
		return
	}

	issue = cloneIssue(issue)
	state.Running[issue.ID] = Running{
		Issue:      issue,
		Attempt:    attempt,
		StartedAt:  now,
		WorkerHost: workerHost,
	}
	state.Claimed[issue.ID] = Claimed{
		Issue:     issue,
		ClaimedAt: now,
	}
	delete(state.Retry, issue.ID)
	delete(state.Blocked, issue.ID)
	delete(state.BudgetRefusals, issue.ID)

	request := RunRequest{
		Issue:      issue,
		Attempt:    attempt,
		StartedAt:  now,
		WorkerHost: workerHost,
	}
	go func() {
		result, err := o.runner.Run(ctx, request)
		event := runResultEvent{
			issueID:     request.Issue.ID,
			result:      result,
			err:         err,
			completedAt: time.Now(),
		}

		select {
		case o.runResults <- event:
		case <-ctx.Done():
		}
	}()
}

func (o *Orchestrator) handleRunResult(state *State, event runResultEvent) {
	running, ok := state.Running[event.issueID]
	if !ok {
		return
	}
	delete(state.Running, event.issueID)

	if event.err != nil {
		o.scheduleRetry(
			state,
			running.Issue,
			nextAttempt(running.Attempt),
			event.completedAt,
			event.err.Error(),
			false,
			running.WorkerHost,
		)
		return
	}

	finalState := event.result.FinalState
	if finalState == "" {
		finalState = FinalStateCompleted
	}

	state.Completed[event.issueID] = Completed{
		Issue:       cloneIssue(running.Issue),
		StartedAt:   running.StartedAt,
		CompletedAt: event.completedAt,
		FinalState:  finalState,
		Tokens:      event.result.Tokens,
	}
	state.CodexTotals = addCodexTotals(state.CodexTotals, event.result.Tokens)
	if event.result.RateLimits != nil {
		state.RateLimits = cloneRateLimits(event.result.RateLimits)
	}
	if diffStatsPresent(event.result.DiffStats) {
		state.DiffStats[event.issueID] = event.result.DiffStats
	}
	if event.result.BudgetRefusal != nil {
		refusal := *event.result.BudgetRefusal
		refusal.Issue = cloneIssue(running.Issue)
		state.BudgetRefusals[event.issueID] = refusal
	}

	o.scheduleRetry(state, running.Issue, 1, event.completedAt, "", true, running.WorkerHost)
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

	delay := o.retryDelay(attempt, continuation)
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
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Blocked, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
}

func (o *Orchestrator) releaseClaim(state *State, issueID string) {
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
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

	cfg.ActiveStates = normalizedStates(cfg.ActiveStates)
	cfg.TerminalStates = normalizedStates(cfg.TerminalStates)
	cfg.MaxConcurrentAgentsByState = cloneStateLimits(cfg.MaxConcurrentAgentsByState)
	cfg.DispatchPriorityByState = normalizedStates(cfg.DispatchPriorityByState)
	cfg.AutoPromote = normalizeAutoPromoteConfig(cfg.AutoPromote)
	cfg.WorkerHosts = normalizeWorkerHosts(cfg.WorkerHosts)
	if cfg.MaxConcurrentAgentsPerHost < 0 {
		cfg.MaxConcurrentAgentsPerHost = 0
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

func todoBlockedByNonTerminal(issue connector.Issue, terminalStates []string) bool {
	if normalizeState(issue.State) != "todo" {
		return false
	}

	for _, blocker := range issue.BlockedBy {
		if blocker.State == "" || !stateIn(blocker.State, terminalStates) {
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
