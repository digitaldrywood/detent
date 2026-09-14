package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestAttemptAllowanceCountsIssueJourney(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	base := []store.WorkAttempt{
		{ID: 1, WorkerType: "agent", Lane: "Todo", StartedAt: now, WorkerMetadataJSON: `{"head":"one"}`},
		{ID: 2, WorkerType: "agent", Lane: "Rework", StartedAt: now.Add(time.Minute), WorkerMetadataJSON: `{"head":"two"}`},
		{ID: 3, WorkerType: "agent", Lane: "In Progress", StartedAt: now.Add(2 * time.Minute), WorkerMetadataJSON: `{"head":"three"}`},
	}
	tests := []struct {
		name      string
		attempts  []store.WorkAttempt
		mergedAt  time.Time
		count     int
		exhausted bool
		triage    int64
	}{
		{name: "two code sessions permit another", attempts: base[:2], count: 2},
		{name: "three code and rework sessions refuse fourth despite new heads and lanes", attempts: base, count: 3, exhausted: true},
		{name: "merge resets prior sessions", attempts: base, mergedAt: now.Add(time.Minute), count: 1},
		{name: "merge after all sessions", attempts: base, mergedAt: now.Add(3 * time.Minute)},
		{name: "triage is recorded but not charged", attempts: append(append([]store.WorkAttempt{}, base...), store.WorkAttempt{ID: 4, WorkerType: runpkg.RunModeTriage, StartedAt: now.Add(3 * time.Minute)}), count: 3, exhausted: true, triage: 4},
		{name: "validator and merge runs are not code sessions", attempts: []store.WorkAttempt{{WorkerType: "validator"}, {WorkerType: "merge"}, {WorkerType: "planner"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countSessionsWithoutMerge(tt.attempts, tt.mergedAt)
			var triage int64
			if got.Triage != nil {
				triage = got.Triage.ID
			}
			if got.Sessions != tt.count || got.exhausted() != tt.exhausted || triage != tt.triage {
				t.Fatalf("allowance = %#v, triage %d", got, triage)
			}
		})
	}
	for _, class := range []string{"backend_startup_failure", "backend_startup_timeout", "transport_error", "backend_protocol_error", "service_restart", "workspace_preparation", "tracker_unavailable", "forge_unavailable"} {
		t.Run(class+" does not consume allowance", func(t *testing.T) {
			attempts := append([]store.WorkAttempt{}, base...)
			attempts[2].ErrorClass = class
			if got := countSessionsWithoutMerge(attempts, time.Time{}); got.Sessions != 2 || got.exhausted() {
				t.Fatalf("allowance = %#v", got)
			}
		})
	}
}

type attemptTriageConnector struct{ implementProgressConnector }

func (c *attemptTriageConnector) CreateComment(ctx context.Context, id, body string) error {
	c.refreshed.Comments = append(c.refreshed.Comments, connector.IssueComment{Body: body})
	return c.implementProgressConnector.CreateComment(ctx, id, body)
}

func TestAttemptAllowanceTriagePublication(t *testing.T) {
	t.Parallel()
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%v", interrupted), func(t *testing.T) {
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "In Progress"}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
			cfg := laneMutationTestConfig()
			db, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			state := newState(cfg)
			note := "## Why this stalled\nThree sessions kept failing the same CI check.\n\n## What is blocking\n- [Failing check](https://github.com/owner/repo/pull/2/checks) needs a fix.\n\n## Options\n- Fix the failing check manually.\n- Close the PR and reduce the scope."
			attempt := store.WorkAttempt{ID: 4, WorkerType: runpkg.RunModeTriage, WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"attempt_allowance_triage": note})}
			if interrupted {
				attempt.WorkerMetadataJSON = "{}"
			}
			for range 2 {
				if err := orch.publishAttemptTriage(t.Context(), &state, issue, attempt, now); err != nil {
					t.Fatal(err)
				}
				issue.State = autoPromoteSourceState
			}
			if len(tracker.comments) != 1 {
				t.Fatalf("comments = %d, want exactly one", len(tracker.comments))
			}
			body := strings.Split(tracker.comments[0].body, "\n\n<!--")[0]
			if !validAttemptTriageNote(body) {
				t.Fatalf("invalid note: %s", body)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != "Human Review" {
				t.Fatalf("updates = %#v", tracker.updates)
			}
			timeline, err := db.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range timeline.Events {
				found = found || event.Reason == attemptAllowanceExhaustedReason
			}
			if !found {
				t.Fatal("missing allowance transition reason")
			}
		})
	}
}

func TestAttemptAllowanceNoteFormat(t *testing.T) {
	t.Parallel()
	valid := "## Why this stalled\nCI kept failing.\n## What is blocking\n- [CI](https://example.com/check) is red.\n## Options\n- Fix CI manually.\n- Close the issue."
	for _, tt := range []struct {
		name, note string
		valid      bool
	}{
		{"valid", valid, true}, {"empty", "", false},
		{"extra prose", "Here is the result:\n" + valid, false},
		{"unlinked blocker", strings.ReplaceAll(valid, "[CI](https://example.com/check)", "CI"), false},
		{"one option", strings.ReplaceAll(valid, "\n- Close the issue.", ""), false},
		{"too many explanation lines", strings.ReplaceAll(valid, "CI kept failing.", "One.\nTwo.\nThree.\nFour."), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := validAttemptTriageNote(tt.note); got != tt.valid {
				t.Fatalf("valid = %v, want %v", got, tt.valid)
			}
		})
	}
}

type attemptTriageRunner struct{}

func (attemptTriageRunner) Run(_ context.Context, req runpkg.RunRequest) (runpkg.RunResult, error) {
	return runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: fallbackAttemptTriageNote(req.Issue, "test evidence")}, nil
}

func TestAttemptAllowanceDispatchAndRestart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		sessions int
		infra    bool
		wantMode string
	}{
		{"third code session permitted", 2, false, runpkg.RunModeImplement},
		{"fourth code session replaced by one triage", 3, false, runpkg.RunModeTriage},
		{"infra failure leaves a session", 3, true, runpkg.RunModeImplement},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := connector.Issue{ID: "stalled", Identifier: "owner/repo#2595", URL: "https://github.com/owner/repo/issues/2595", State: "Rework"}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now.Add(-time.Hour))
			for i := range tt.sessions {
				if i > 0 {
					var err error
					id, err = db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: "Rework", AttemptNumber: i + 1, StartedAt: now.Add(-time.Duration(10-i) * time.Minute)})
					if err != nil {
						t.Fatal(err)
					}
				}
				class := ""
				if tt.infra && i == tt.sessions-1 {
					class = "service_restart"
				}
				if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(-time.Duration(9-i) * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess, ErrorClass: class}); err != nil {
					t.Fatal(err)
				}
			}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			orch.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
			orch.runResults = make(chan runpkg.Completion, 1)
			state := newState(cfg)
			if !orch.dispatchIssue(t.Context(), &state, issue, tt.sessions+1, now, "") {
				t.Fatal("dispatch refused")
			}
			var result runpkg.Completion
			select {
			case result = <-orch.runResults:
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not complete")
			}
			if result.Request.Mode != tt.wantMode {
				t.Fatalf("mode = %s, want %s", result.Request.Mode, tt.wantMode)
			}
			if tt.wantMode != runpkg.RunModeTriage {
				return
			}
			if len(result.Request.AgentTools) != 0 || result.Request.AgentToolHandler != nil || !strings.Contains(result.Request.TriageContext, "PriorSessions") {
				t.Fatalf("triage request = %#v", result.Request)
			}
			orch.handleRunResult(t.Context(), &state, result)
			if len(tracker.comments) != 1 || len(tracker.updates) != 1 || tracker.updates[0].state != "Human Review" {
				t.Fatalf("tracker writes: comments=%#v updates=%#v", tracker.comments, tracker.updates)
			}
			issue.State = "Human Review"
			tracker.refreshed = issue
			tracker.refreshed.Comments = []connector.IssueComment{{Body: tracker.comments[0].body}}
			// New runtime, same durable attempt log: no repeat triage or fourth code session.
			restarted := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			restarted.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
			restartedState := newState(cfg)
			if restarted.dispatchIssue(t.Context(), &restartedState, issue, 5, now, "") {
				t.Fatal("exhausted issue redispatched after restart")
			}
			if len(tracker.comments) != 1 || len(restartedState.Running) != 0 {
				t.Fatal("triage repeated after restart")
			}
		})
	}
}

func TestAttemptAllowanceMergeTimeAndRunningOwnership(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	merged := now.Add(-time.Hour)
	for _, tt := range []struct {
		name string
		pr   *connector.PullRequest
		want time.Time
	}{
		{"repeated historical observation", nil, merged},
		{"immutable provider time refines observation", &connector.PullRequest{Number: 1, State: "MERGED", MergedAt: new(merged.Add(-time.Minute))}, merged.Add(-time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := []store.WorkflowPhaseEvent{
				{PhaseType: store.WorkflowPhaseTypeLane, Status: "entered", Reason: "pull_request_merged", PRNumber: new(int64(1)), StartedAt: now},
				{PhaseType: store.WorkflowPhaseTypeLane, Status: "entered", Reason: "pull_request_merged", PRNumber: new(int64(1)), StartedAt: merged},
			}
			if got := lastAllowanceMergeAt(connector.Issue{PullRequest: tt.pr}, events); !got.Equal(tt.want) {
				t.Fatalf("merge boundary = %s, want %s", got, tt.want)
			}
		})
	}
	for _, tt := range []struct {
		name     string
		mergedAt *time.Time
		want     int
	}{
		{"immutable merge resets old sessions only", &merged, 2},
		{"activity alone is not a merge", nil, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := connector.Issue{ID: "issue", PullRequest: &connector.PullRequest{State: "MERGED", MergedAt: tt.mergedAt, ActivityAt: &now}}
			attempts := &recordingWorkAttemptStore{history: []store.WorkAttempt{
				{WorkerType: "agent", StartedAt: now.Add(-2 * time.Hour)},
				{WorkerType: "agent", StartedAt: now.Add(-30 * time.Minute)},
				{WorkerType: "agent", StartedAt: now.Add(-10 * time.Minute)},
			}}
			orch := &Orchestrator{workAttempts: attempts}
			got, err := orch.issueAttemptAllowance(t.Context(), issue)
			if err != nil || got.Sessions != tt.want {
				t.Fatalf("allowance = %#v, error = %v", got, err)
			}
		})
	}
	issue := connector.Issue{ID: "stalled", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "Human Review"}
	cfg := laneMutationTestConfig()
	db, activeID := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
	for i := range 2 {
		id, err := db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, WorkerType: "agent", AttemptNumber: i + 1, StartedAt: now.Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(-time.Minute), TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
	orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: activeID, Mode: runpkg.RunModeImplement}
	orch.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)
	if state.Running[issue.ID].WorkAttemptID != activeID || len(tracker.comments) != 0 || len(tracker.updates) != 0 {
		t.Fatal("triage replaced the third running worker")
	}
}

func TestAttemptAllowancePreservesOperatorCompletionLane(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Done", "Blocked"} {
		t.Run(lane, func(t *testing.T) {
			now := time.Now().UTC()
			issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: lane}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			state := newState(cfg)
			running := Running{Issue: issue, WorkAttemptID: id, Mode: runpkg.RunModeTriage, CompletionLane: lane}
			orch.finishAttemptTriage(t.Context(), &state, runpkg.Completion{CompletedAt: now, Result: runpkg.RunResult{Output: fallbackAttemptTriageNote(issue, "test")}}, running)
			attempt, err := db.WorkAttempt(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err := orch.publishAttemptTriage(t.Context(), &state, issue, attempt, now); err != nil {
				t.Fatal(err)
			}
			if len(tracker.comments) != 1 || len(tracker.updates) != 0 {
				t.Fatalf("operator lane overridden: comments=%d updates=%#v", len(tracker.comments), tracker.updates)
			}
		})
	}
}
