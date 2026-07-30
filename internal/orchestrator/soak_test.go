package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

const soakEnvironment = "DETENT_RUN_SOAK_TESTS"

func TestSoakHistoricalNoPRIncidentReplay(t *testing.T) {
	requireSoak(t)

	fixture := loadNoPRIncidentFixture(t)
	for _, incident := range fixture.Incidents {
		t.Run(incident.Identifier, func(t *testing.T) {
			if len(incident.Attempts) != incident.ExpectedSessions {
				t.Fatalf("attempts = %d, want %d historical sessions", len(incident.Attempts), incident.ExpectedSessions)
			}
			var fixtureTokens int64
			for _, attempt := range incident.Attempts {
				fixtureTokens += attempt.Tokens.Total
				if attempt.FinalState != FinalStateCompleted {
					t.Fatalf("session %d final_state = %q, want completed", attempt.SessionID, attempt.FinalState)
				}
				if attempt.PullRequestPresent {
					t.Fatalf("session %d unexpectedly records a pull request", attempt.SessionID)
				}
			}
			if fixtureTokens != incident.ExpectedTotalTokens {
				t.Fatalf("fixture tokens = %d, want %d", fixtureTokens, incident.ExpectedTotalTokens)
			}

			const noProgressLimit = 3
			issue := connector.Issue{
				ID:               strings.ReplaceAll(incident.Identifier, "/", "-"),
				Identifier:       incident.Identifier,
				Title:            "Historical no-PR loop replay",
				State:            "In Progress",
				AssignedToWorker: true,
			}
			tracker := newSoakConnector(issue)
			attempts := newSoakAttemptStore()
			cfg := normalizeConfig(Config{
				Project:        scheduler.ProjectCandidate{ID: "gopher-ai"},
				AutoPromote:    AutoPromoteConfig{NoProgressLimit: noProgressLimit},
				ActiveStates:   []string{"Todo", "In Progress", "Rework"},
				ObservedStates: []string{"Blocked"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			state := newState(cfg)
			breakerAttempt := 0

			for index, historical := range incident.Attempts {
				startedAt := mustParseSoakTime(t, historical.StartedAt)
				completedAt := mustParseSoakTime(t, historical.CompletedAt)
				attemptID, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{
					ProjectID:     "gopher-ai",
					IssueID:       issue.ID,
					Identifier:    issue.Identifier,
					WorkerType:    "agent",
					AttemptNumber: index + 1,
					StartedAt:     startedAt,
				})
				if err != nil {
					t.Fatalf("start replay attempt: %v", err)
				}
				diff := historical.DiffStats.runnerDiff(incident.FunctionalDiffGroup)
				state.Running[issue.ID] = Running{
					Issue:         issue,
					Attempt:       index + 1,
					WorkAttemptID: attemptID,
					Mode:          runpkg.RunModeImplement,
					StartedAt:     startedAt,
					DiffStats:     diff,
				}
				state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: startedAt}
				delete(state.Retry, issue.ID)
				delete(state.Completed, issue.ID)

				orch.handleRunResult(t.Context(), &state, runpkg.Completion{
					IssueID:     issue.ID,
					CompletedAt: completedAt,
					Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
					Result: runpkg.RunResult{
						FinalState: historical.FinalState,
						DiffStats:  diff,
						Tokens:     historical.Tokens.runnerTotals(),
					},
				})
				if _, blocked := state.Blocked[issue.ID]; blocked {
					breakerAttempt = index + 1
					break
				}
			}

			if breakerAttempt == 0 || breakerAttempt > noProgressLimit {
				t.Fatalf("breaker attempt = %d, want 1..%d", breakerAttempt, noProgressLimit)
			}
			if got := tracker.issueState(issue.ID); got != blockedStatusState {
				t.Fatalf("tracker state = %q, want %q", got, blockedStatusState)
			}
		})
	}
}

func TestSoakHardPerIssueCostCeilingInvariant(t *testing.T) {
	requireSoak(t)

	const (
		simulatedTicks       = 1_000
		noProgressSpendLimit = 5.00
		costPerSession       = 1.35
		tokensPerSession     = int64(500_000)
		maxSessions          = int64(4)
		maxTokens            = maxSessions * tokensPerSession
	)

	startedAt := time.Date(2026, 7, 11, 16, 0, 0, 0, time.UTC)
	clock := &soakClock{now: startedAt}
	issue := connector.Issue{
		ID:               "synthetic-no-progress",
		Identifier:       "example/soak#1",
		Title:            "Always-successful unmerged diff",
		State:            "In Progress",
		AssignedToWorker: true,
		CreatedAt:        &startedAt,
	}
	tracker := newSoakConnector(issue)
	attempts := newSoakAttemptStore()
	runner := &soakSuccessRunner{
		clock:          clock,
		spend:          attempts,
		costPerSession: costPerSession,
		tokensPerRun:   tokensPerSession,
	}
	cfg := Config{
		PollInterval:                  time.Minute,
		MaxConcurrentAgents:           1,
		BillingMode:                   "metered",
		NoProgressSpendLimitUSD:       noProgressSpendLimit,
		Project:                       scheduler.ProjectCandidate{ID: "soak"},
		AutoPromote:                   AutoPromoteConfig{NoProgressLimit: 0},
		ActiveStates:                  []string{"In Progress", "Rework"},
		ObservedStates:                []string{"Blocked"},
		TerminalStates:                []string{"Done", "Cancelled"},
		ContinuationRetryDelay:        time.Minute,
		FailureRetryBaseDelay:         time.Minute,
		WorkspaceCleanupIdleTTL:       time.Hour,
		WorkspaceCleanupSweepInterval: time.Hour,
	}
	orch, err := New(cfg, Dependencies{
		Connector:     tracker,
		Runner:        runner,
		WorkAttempts:  attempts,
		ProgressSpend: attempts,
		Now:           clock.Now,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)

	for range simulatedTicks {
		now := clock.Advance(time.Minute)
		orch.tick(t.Context(), &state, now)
		if _, blocked := state.Blocked[issue.ID]; blocked {
			break
		}
		completion := receiveSoakCompletion(t, orch.runResults)
		orch.handleRunResult(t.Context(), &state, completion)
		if _, blocked := state.Blocked[issue.ID]; blocked {
			break
		}
	}

	sessions, tokens := attempts.totals()
	if sessions > maxSessions {
		t.Fatalf("sessions = %d, ceiling %d", sessions, maxSessions)
	}
	if tokens > maxTokens {
		t.Fatalf("tokens = %d, ceiling %d", tokens, maxTokens)
	}
	blocked, ok := state.Blocked[issue.ID]
	if !ok {
		t.Fatalf("issue did not reach Blocked after %d simulated ticks; sessions=%d tokens=%d", simulatedTicks, sessions, tokens)
	}
	if blocked.Reason != spendProgressReason {
		t.Fatalf("blocked reason = %q, want %q", blocked.Reason, spendProgressReason)
	}
	if got := tracker.issueState(issue.ID); got != blockedStatusState {
		t.Fatalf("tracker state = %q, want %q", got, blockedStatusState)
	}
}

func requireSoak(t *testing.T) {
	t.Helper()
	if os.Getenv(soakEnvironment) != "1" {
		t.Skip("set DETENT_RUN_SOAK_TESTS=1 to run local orchestrator soak tests")
	}
}

type noPRIncidentFixture struct {
	Schema    int            `json:"schema"`
	Incidents []noPRIncident `json:"incidents"`
}

type noPRIncident struct {
	Identifier          string                `json:"identifier"`
	ExpectedSessions    int                   `json:"expected_sessions"`
	ExpectedTotalTokens int64                 `json:"expected_total_tokens"`
	FunctionalDiffGroup string                `json:"functional_diff_group"`
	Attempts            []noPRIncidentAttempt `json:"attempts"`
}

type noPRIncidentAttempt struct {
	SessionID          int64                  `json:"session_id"`
	WorkAttemptID      int64                  `json:"work_attempt_id"`
	StartedAt          string                 `json:"started_at"`
	CompletedAt        string                 `json:"completed_at"`
	FinalState         string                 `json:"final_state"`
	Tokens             noPRIncidentTokens     `json:"tokens"`
	DiffStats          *noPRIncidentDiffStats `json:"diffstat"`
	PullRequestPresent bool                   `json:"pull_request_present"`
}

type noPRIncidentTokens struct {
	Input           int64 `json:"input"`
	CachedInput     int64 `json:"cached_input"`
	Output          int64 `json:"output"`
	ReasoningOutput int64 `json:"reasoning_output"`
	Total           int64 `json:"total"`
}

func (t noPRIncidentTokens) runnerTotals() TokenTotals {
	return TokenTotals{
		InputTokens:           t.Input,
		CachedInputTokens:     t.CachedInput,
		OutputTokens:          t.Output,
		ReasoningOutputTokens: t.ReasoningOutput,
		TotalTokens:           t.Total,
	}
}

type noPRIncidentDiffStats struct {
	FilesChanged int    `json:"files_changed"`
	AddedLines   int    `json:"added_lines"`
	RemovedLines int    `json:"removed_lines"`
	Status       string `json:"status"`
}

func (d *noPRIncidentDiffStats) runnerDiff(functionalGroup string) DiffStats {
	if d == nil {
		return DiffStats{}
	}
	return DiffStats{
		FilesChanged: d.FilesChanged,
		AddedLines:   d.AddedLines,
		RemovedLines: d.RemovedLines,
		Fingerprint:  functionalGroup,
		Status:       d.Status,
	}
}

func loadNoPRIncidentFixture(t *testing.T) noPRIncidentFixture {
	t.Helper()
	// This checked-in fixture is the literal 2026-07-11 codex_sessions/work_attempts
	// incident export, not generated test data.
	path := filepath.Join("testdata", "incidents", "2026-07-11-gopher-ai-213-214-no-pr-loop.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fixture noPRIncidentFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if fixture.Schema != 1 || len(fixture.Incidents) != 2 {
		t.Fatalf("fixture schema/incidents = %d/%d, want 1/2", fixture.Schema, len(fixture.Incidents))
	}
	return fixture
}

func mustParseSoakTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

type soakClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *soakClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *soakClock) Advance(elapsed time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(elapsed)
	return c.now
}

type soakConnector struct {
	mu       sync.Mutex
	issues   map[string]connector.Issue
	comments map[string][]connector.IssueComment
}

func newSoakConnector(issues ...connector.Issue) *soakConnector {
	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = cloneIssue(issue)
	}
	return &soakConnector{issues: byID, comments: map[string][]connector.IssueComment{}}
}

func (c *soakConnector) Name() string { return "soak" }

func (c *soakConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	issues := make([]connector.Issue, 0, len(c.issues))
	for _, issue := range c.issues {
		issues = append(issues, cloneIssue(issue))
	}
	return issues, nil
}

func (c *soakConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	issues := make([]connector.Issue, 0, len(c.issues))
	for _, issue := range c.issues {
		if stateIn(issue.State, states) {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *soakConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := c.issues[id]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *soakConnector) FetchIssueComments(_ context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.comments[issue.ID]), nil
}

func (c *soakConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comments[issueID] = append(c.comments[issueID], connector.IssueComment{Body: body})
	return nil
}

func (c *soakConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	issue, ok := c.issues[issueID]
	if !ok {
		return errors.New("issue not found")
	}
	issue.State = state
	c.issues[issueID] = issue
	return nil
}

func (c *soakConnector) SetAssignee(context.Context, string, string) error { return nil }

func (c *soakConnector) SetField(context.Context, string, string, string) error { return nil }

func (c *soakConnector) issueState(issueID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issues[issueID].State
}

func (c *soakConnector) commentCount(issueID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.comments[issueID])
}

type soakSpendEntry struct {
	at     time.Time
	cost   float64
	tokens int64
}

type soakAttemptStore struct {
	mu      sync.Mutex
	nextID  int64
	starts  map[int64]store.WorkAttemptStart
	history []store.WorkAttempt
	spend   []soakSpendEntry
}

func newSoakAttemptStore() *soakAttemptStore {
	return &soakAttemptStore{starts: map[int64]store.WorkAttemptStart{}}
}

func (s *soakAttemptStore) StartWorkAttempt(_ context.Context, start store.WorkAttemptStart) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.starts[s.nextID] = start
	return s.nextID, nil
}

func (s *soakAttemptStore) WorkAttempt(_ context.Context, id int64) (store.WorkAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, attempt := range s.history {
		if attempt.ID == id {
			return attempt, nil
		}
	}
	return store.WorkAttempt{}, store.ErrNotFound
}

func (s *soakAttemptStore) RecordWorkAttemptHeartbeat(context.Context, store.WorkAttemptHeartbeat) error {
	return nil
}

func (s *soakAttemptStore) CompleteWorkAttempt(_ context.Context, completion store.WorkAttemptCompletion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	start, ok := s.starts[completion.AttemptID]
	if !ok {
		return errors.New("attempt start not found")
	}
	attempt := store.WorkAttempt{
		ID:                 completion.AttemptID,
		ProjectID:          start.ProjectID,
		IssueID:            start.IssueID,
		Identifier:         start.Identifier,
		IssueURL:           start.IssueURL,
		WorkerType:         start.WorkerType,
		AttemptNumber:      start.AttemptNumber,
		StartedAt:          start.StartedAt,
		CompletedAt:        completion.CompletedAt,
		Status:             completion.Status,
		TerminalState:      completion.TerminalState,
		ErrorClass:         completion.ErrorClass,
		WorkerMetadataJSON: completion.WorkerMetadataJSON,
	}
	s.history = append([]store.WorkAttempt{attempt}, s.history...)
	delete(s.starts, completion.AttemptID)
	return nil
}

func (s *soakAttemptStore) ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *soakAttemptStore) ListRecentTerminalWorkAttempts(_ context.Context, query store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := make([]store.WorkAttempt, 0, len(s.history))
	for _, attempt := range s.history {
		if query.IssueID != "" && attempt.IssueID != query.IssueID {
			continue
		}
		if query.Identifier != "" && attempt.Identifier != query.Identifier {
			continue
		}
		if query.WorkerType != "" && attempt.WorkerType != query.WorkerType {
			continue
		}
		filtered = append(filtered, attempt)
		if query.Limit > 0 && len(filtered) >= query.Limit {
			break
		}
	}
	return filtered, nil
}

func (s *soakAttemptStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *soakAttemptStore) ReclaimActiveWorkAttempts(context.Context, store.WorkAttemptReclaim) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *soakAttemptStore) RecordSchedulerDecision(context.Context, store.SchedulerDecision) (int64, error) {
	return 0, nil
}

func (s *soakAttemptStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}

func (s *soakAttemptStore) IssueSpendSince(_ context.Context, query store.IssueSpendSinceQuery) (store.IssueSpendSince, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := store.IssueSpendSince{}
	for _, entry := range s.spend {
		if entry.at.Before(query.Since) {
			continue
		}
		result.CostUSD += entry.cost
		result.Sessions++
		if result.FirstSessionAt.IsZero() || entry.at.Before(result.FirstSessionAt) {
			result.FirstSessionAt = entry.at
		}
		if entry.at.After(result.LastSessionAt) {
			result.LastSessionAt = entry.at
		}
	}
	return result, nil
}

func (s *soakAttemptStore) recordSpend(at time.Time, cost float64, tokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spend = append(s.spend, soakSpendEntry{at: at, cost: cost, tokens: tokens})
}

func (s *soakAttemptStore) totals() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tokens int64
	for _, entry := range s.spend {
		tokens += entry.tokens
	}
	return int64(len(s.spend)), tokens
}

type soakSuccessRunner struct {
	mu             sync.Mutex
	clock          *soakClock
	spend          *soakAttemptStore
	costPerSession float64
	tokensPerRun   int64
	calls          int
}

func (r *soakSuccessRunner) Run(context.Context, RunRequest) (RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.spend.recordSpend(r.clock.Now(), r.costPerSession, r.tokensPerRun)
	return RunResult{
		FinalState: FinalStateCompleted,
		Tokens:     TokenTotals{InputTokens: r.tokensPerRun - 1, OutputTokens: 1, TotalTokens: r.tokensPerRun},
		DiffStats: DiffStats{
			FilesChanged: 1,
			AddedLines:   r.calls,
			Fingerprint:  "varying-diff-" + time.Duration(r.calls).String(),
			Status:       "changed",
		},
	}, nil
}

func (r *soakSuccessRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func receiveSoakCompletion(t *testing.T, completions <-chan runpkg.Completion) runpkg.Completion {
	t.Helper()
	select {
	case completion := <-completions:
		return completion
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for synthetic runner completion")
		return runpkg.Completion{}
	}
}
