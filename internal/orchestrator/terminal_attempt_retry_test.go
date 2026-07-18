package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestHandleRunResultRoutesTerminalRetryByWorkProduct(t *testing.T) {
	t.Parallel()

	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	capacityReset := time.Date(2026, 7, 18, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		issue           connector.Issue
		result          runpkg.RunResult
		runError        error
		wantState       string
		wantTransitions []string
	}{
		{
			name:            "no pushed work returns to todo",
			issue:           terminalRetryTestIssue("empty"),
			runError:        errors.New("runner failed"),
			wantState:       "Todo",
			wantTransitions: []string{"Todo"},
		},
		{
			name:            "transient overload returns to todo",
			issue:           terminalRetryTestIssue("overload"),
			runError:        backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: "serverOverloaded"}, errors.New("provider overloaded")),
			wantState:       "Todo",
			wantTransitions: []string{"Todo"},
		},
		{
			name:            "startup timeout returns to todo",
			issue:           terminalRetryTestIssue("startup-timeout"),
			runError:        backendcapacity.NewError(scope, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: backendcapacity.StartupTimeoutKind}, context.DeadlineExceeded),
			wantState:       "Todo",
			wantTransitions: []string{"Todo"},
		},
		{
			name:            "provider capacity returns to todo",
			issue:           terminalRetryTestIssue("capacity"),
			runError:        backendcapacity.NewError(scope, backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &capacityReset}, errors.New("provider usage limit reached")),
			wantState:       "Todo",
			wantTransitions: []string{"Todo"},
		},
		{
			name:      "pushed branch keeps in progress",
			issue:     terminalRetryTestIssue("pushed"),
			result:    runpkg.RunResult{PullRequestHeadPushed: true},
			runError:  errors.New("runner failed"),
			wantState: "In Progress",
		},
		{
			name:      "linked pull request keeps in progress",
			issue:     terminalRetryTestIssueWithPullRequest("pull-request"),
			runError:  errors.New("runner failed"),
			wantState: "In Progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{tt.issue.ID: cloneIssue(tt.issue)}}
			attempts := &terminalRetryWorkAttemptStore{}
			cfg := normalizeConfig(Config{
				ActiveStates:          []string{"Todo", "In Progress"},
				TerminalStates:        []string{"Done"},
				MaxRetryBackoff:       time.Minute,
				FailureRetryBaseDelay: time.Second,
			})
			o := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			state := newState(cfg)
			state.Running[tt.issue.ID] = Running{
				Issue:         cloneIssue(tt.issue),
				Attempt:       1,
				WorkAttemptID: 42,
				Mode:          runpkg.RunModeImplement,
				StartedAt:     now.Add(-time.Minute),
			}
			state.Claimed[tt.issue.ID] = Claimed{Issue: cloneIssue(tt.issue), ClaimedAt: now.Add(-time.Minute)}

			o.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID:      tt.issue.ID,
				Result:       tt.result,
				Err:          tt.runError,
				CompletedAt:  now,
				RetryAttempt: 2,
				RetryDelay:   time.Minute,
			})

			retry, ok := state.Retry[tt.issue.ID]
			if !ok {
				t.Fatalf("Retry[%q] missing", tt.issue.ID)
			}
			if retry.Issue.State != tt.wantState {
				t.Fatalf("Retry[%q].Issue.State = %q, want %q", tt.issue.ID, retry.Issue.State, tt.wantState)
			}
			if _, claimed := state.Claimed[tt.issue.ID]; claimed {
				t.Fatalf("Claimed[%q] present after terminal completion", tt.issue.ID)
			}
			if got := tracker.transitionStates(); !slices.Equal(got, tt.wantTransitions) {
				t.Fatalf("state transitions = %v, want %v", got, tt.wantTransitions)
			}
			if len(attempts.completions) != 1 {
				t.Fatalf("work attempt completions = %d, want 1", len(attempts.completions))
			}
			if got := terminalRetryMetadataPushed(attempts.completions[0].WorkerMetadataJSON); got != tt.result.PullRequestHeadPushed {
				t.Fatalf("persisted work_product_pushed = %v, want %v", got, tt.result.PullRequestHeadPushed)
			}
		})
	}
}

func TestReconcileTerminalAttemptRetryStatesDemotesRecoveredEmptyAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	empty := terminalRetryTestIssue("service-restart-empty")
	pushed := terminalRetryTestIssue("service-restart-pushed")
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{
		empty.ID:  cloneIssue(empty),
		pushed.ID: cloneIssue(pushed),
	}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
	o := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.WorkAttempts = []telemetry.WorkAttempt{
		{
			AttemptID:          2,
			IssueID:            pushed.ID,
			Identifier:         pushed.Identifier,
			Status:             string(store.WorkAttemptStatusTerminal),
			TerminalState:      string(store.WorkAttemptTerminalAbandoned),
			ErrorClass:         "service_restart",
			CompletedAt:        timePointer(now.Add(-time.Minute)),
			WorkerMetadataJSON: `{"work_product_pushed":true}`,
		},
		{
			AttemptID:     1,
			IssueID:       empty.ID,
			Identifier:    empty.Identifier,
			Status:        string(store.WorkAttemptStatusTerminal),
			TerminalState: string(store.WorkAttemptTerminalAbandoned),
			ErrorClass:    "service_restart",
			CompletedAt:   timePointer(now.Add(-2 * time.Minute)),
		},
	}

	transitions := o.reconcileTerminalAttemptRetryStates(t.Context(), &state, []connector.Issue{pushed, empty}, now)

	if len(transitions) != 1 || transitions[0].ID != empty.ID || transitions[0].State != "Todo" {
		t.Fatalf("transitions = %#v, want empty attempt moved to Todo", transitions)
	}
	if got := tracker.transitionStates(); !slices.Equal(got, []string{"Todo"}) {
		t.Fatalf("state transitions = %v, want [Todo]", got)
	}
}

func TestTerminalAttemptDemotionLetsUrgentTodoRankFirst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	retrying := terminalRetryTestIssue("retrying")
	urgent := terminalRetryTestIssue("urgent")
	urgent.State = "Todo"
	urgentPriority := 1
	urgent.Priority = &urgentPriority
	tracker := &terminalRetryConnector{issues: map[string]connector.Issue{retrying.ID: cloneIssue(retrying), urgent.ID: cloneIssue(urgent)}}
	cfg := normalizeConfig(Config{
		ActiveStates:            []string{"Todo", "In Progress"},
		TerminalStates:          []string{"Done"},
		DispatchPriorityByState: []string{"In Progress", "Todo"},
	})
	o := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.WorkAttempts = []telemetry.WorkAttempt{{
		AttemptID:     1,
		IssueID:       retrying.ID,
		Identifier:    retrying.Identifier,
		Status:        string(store.WorkAttemptStatusTerminal),
		TerminalState: string(store.WorkAttemptTerminalFailure),
		ErrorClass:    "runner_error",
		CompletedAt:   timePointer(now.Add(-time.Minute)),
	}}

	transitions := o.reconcileTerminalAttemptRetryStates(t.Context(), &state, []connector.Issue{retrying, urgent}, now)
	issues := overlayIssueStateSnapshots([]connector.Issue{retrying, urgent}, transitions)
	sortIssuesForDispatch(issues, cfg.DispatchPriorityByState, cfg.DispatchPriorityByLabel, false)

	if len(issues) != 2 || issues[0].ID != urgent.ID {
		t.Fatalf("dispatch order = %v, want urgent Todo first", []string{issues[0].ID, issues[1].ID})
	}
}

func terminalRetryTestIssue(suffix string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "issue-" + suffix
	issue.Identifier = "digitaldrywood/detent#1432-" + suffix
	issue.Title = "Terminal retry " + suffix
	issue.State = "In Progress"
	return issue
}

func terminalRetryTestIssueWithPullRequest(suffix string) connector.Issue {
	issue := terminalRetryTestIssue(suffix)
	prNumber := 1432
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{Number: prNumber, State: "OPEN"}
	return issue
}

type terminalRetryConnector struct {
	issues      map[string]connector.Issue
	transitions []string
}

func (c *terminalRetryConnector) Name() string { return "terminal-retry" }

func (c *terminalRetryConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *terminalRetryConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *terminalRetryConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := c.issues[id]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *terminalRetryConnector) CreateComment(context.Context, string, string) error { return nil }

func (c *terminalRetryConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	issue := c.issues[issueID]
	issue.State = state
	c.issues[issueID] = issue
	c.transitions = append(c.transitions, state)
	return nil
}

func (c *terminalRetryConnector) SetAssignee(context.Context, string, string) error { return nil }

func (c *terminalRetryConnector) SetField(context.Context, string, string, string) error { return nil }

func (c *terminalRetryConnector) transitionStates() []string {
	return append([]string(nil), c.transitions...)
}

type terminalRetryWorkAttemptStore struct {
	completions []store.WorkAttemptCompletion
}

func (s *terminalRetryWorkAttemptStore) StartWorkAttempt(context.Context, store.WorkAttemptStart) (int64, error) {
	return 0, nil
}

func (s *terminalRetryWorkAttemptStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return store.WorkAttempt{}, store.ErrNotFound
}

func (s *terminalRetryWorkAttemptStore) RecordWorkAttemptHeartbeat(context.Context, store.WorkAttemptHeartbeat) error {
	return nil
}

func (s *terminalRetryWorkAttemptStore) CompleteWorkAttempt(_ context.Context, completion store.WorkAttemptCompletion) error {
	s.completions = append(s.completions, completion)
	return nil
}

func (s *terminalRetryWorkAttemptStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *terminalRetryWorkAttemptStore) ReclaimActiveWorkAttempts(context.Context, store.WorkAttemptReclaim) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *terminalRetryWorkAttemptStore) ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *terminalRetryWorkAttemptStore) ListRecentTerminalWorkAttempts(context.Context, store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *terminalRetryWorkAttemptStore) RecordSchedulerDecision(context.Context, store.SchedulerDecision) (int64, error) {
	return 0, nil
}

func (s *terminalRetryWorkAttemptStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}

func terminalRetryMetadataPushed(raw string) bool {
	return workAttemptMetadataHasPushedProduct(raw)
}
