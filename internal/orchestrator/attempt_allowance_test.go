package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestAttemptAllowanceExcludesMergeRouting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		merge store.WorkAttempt
		want  int
	}{
		{name: "merge worker type", merge: store.WorkAttempt{WorkerType: "merge"}, want: 1},
		{name: "merge run mode stored as agent", merge: store.WorkAttempt{WorkerType: "agent", WorkerMetadataJSON: `{"run_mode":"merge"}`}, want: 1},
		{name: "Merging lane agent", merge: store.WorkAttempt{WorkerType: "agent", Lane: "Merging", WorkerMetadataJSON: `{"run_mode":"merge"}`}, want: 1},
		{name: "Rework conflict repair without progress", merge: store.WorkAttempt{WorkerType: "agent", Lane: "Rework", WorkerMetadataJSON: `{"run_mode":"merge"}`, ErrorClass: "no_progress"}, want: 3},
		{name: "In Progress conflict repair without progress", merge: store.WorkAttempt{WorkerType: "agent", Lane: "In Progress", WorkerMetadataJSON: `{"run_mode":"merge"}`, ErrorClass: "no_progress"}, want: 3},
		{name: "successful conflict repair", merge: store.WorkAttempt{WorkerType: "agent", Lane: "Rework", WorkerMetadataJSON: `{"run_mode":"merge"}`, TerminalState: store.WorkAttemptTerminalSuccess}, want: 3},
		{name: "historical successful routing receipt", merge: store.WorkAttempt{WorkerType: "agent", Phase: "rework", StatusMessage: "merge worker routed current head to Rework"}, want: 1},
		{name: "historical timed out routing receipt", merge: store.WorkAttempt{WorkerType: "agent", TerminalState: store.WorkAttemptTerminalTimedOut, Phase: "rework", StatusMessage: "merge worker routed current head to Rework", ErrorClass: mergeFallbackRequiresReworkReason}, want: 1},
		{name: "three implementation sessions", merge: store.WorkAttempt{WorkerType: "agent", WorkerMetadataJSON: `{"run_mode":"implement"}`, ErrorClass: "no_progress"}, want: 3},
		{name: "rework phase alone is implementation", merge: store.WorkAttempt{WorkerType: "agent", Phase: "rework"}, want: 3},
		{name: "malformed metadata is not merge evidence", merge: store.WorkAttempt{WorkerType: "agent", WorkerMetadataJSON: `{`}, want: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
			attempts := []store.WorkAttempt{tt.merge, tt.merge,
				{WorkerType: "agent", ErrorClass: "no_progress", StatusMessage: "worker completed without PR progress"},
				{WorkerType: runpkg.RunModeTriage},
			}
			for i := range attempts {
				attempts[i].ID = int64(9178 + i)
				attempts[i].StartedAt = now.Add(time.Duration(i) * time.Minute)
				if attempts[i].TerminalState == "" {
					attempts[i].TerminalState = store.WorkAttemptTerminalSuccess
				}
			}
			got := countSessionsWithoutMerge(attempts, time.Time{}, time.Time{})
			if got.Sessions != tt.want || len(got.Attempts) != tt.want || got.exhausted() != (tt.want == 3) {
				t.Fatalf("sessions=%d retained=%d exhausted=%v; want %d sessions", got.Sessions, len(got.Attempts), got.exhausted(), tt.want)
			}
			if got.Triage == nil || got.Triage.ID != attempts[3].ID {
				t.Fatal("read-only triage receipt was not preserved")
			}
		})
	}
}

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
		resetAt   time.Time
		count     int
		exhausted bool
		triage    int64
	}{
		{name: "two code sessions permit another", attempts: base[:2], count: 2},
		{name: "three code and rework sessions refuse fourth despite new heads and lanes", attempts: base, count: 3, exhausted: true},
		{name: "merge resets prior sessions", attempts: base, mergedAt: now.Add(time.Minute), count: 1},
		{name: "reset includes same tick session", attempts: base, resetAt: now, count: 3, exhausted: true},
		{name: "reset excludes earlier sessions", attempts: base, resetAt: now.Add(time.Minute), count: 2},
		{name: "reset after merge includes same tick session", attempts: base, mergedAt: now, resetAt: now.Add(time.Minute), count: 2},
		{name: "merge after reset excludes same tick session", attempts: base, mergedAt: now.Add(time.Minute), resetAt: now, count: 1},
		{name: "simultaneous merge and reset preserves merge boundary", attempts: base, mergedAt: now.Add(time.Minute), resetAt: now.Add(time.Minute), count: 1},
		{name: "merge after all sessions", attempts: base, mergedAt: now.Add(3 * time.Minute)},
		{name: "triage is recorded but not charged", attempts: append(append([]store.WorkAttempt{}, base...), store.WorkAttempt{ID: 4, WorkerType: runpkg.RunModeTriage, StartedAt: now.Add(3 * time.Minute)}), count: 3, exhausted: true, triage: 4},
		{name: "external wait does not erase completed triage", attempts: append(append([]store.WorkAttempt{}, base...), store.WorkAttempt{ID: 4, WorkerType: runpkg.RunModeTriage, WorkerMetadataJSON: `{"allowance_external_wait":true}`}), count: 3, exhausted: true, triage: 4},
		{name: "validator and merge runs are not code sessions", attempts: []store.WorkAttempt{{WorkerType: "validator"}, {WorkerType: "merge"}, {WorkerType: "planner"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countSessionsWithoutMerge(tt.attempts, tt.mergedAt, tt.resetAt)
			var triage int64
			if got.Triage != nil {
				triage = got.Triage.ID
			}
			if got.Sessions != tt.count || got.exhausted() != tt.exhausted || triage != tt.triage {
				t.Fatalf("allowance = %#v, triage %d", got, triage)
			}
		})
	}
	for _, tt := range []struct {
		name     string
		metadata string
		count    int
	}{
		{"instance blocker success", `{"blocker_evidence":[{"owner":"instance"}]}`, 2},
		{"mixed ownership with instance", `{"blocker_evidence":[{"owner":"human"},{"owner":"instance"},{"owner":"orchestrator"}]}`, 2},
		{"human blocker only", `{"blocker_evidence":[{"owner":"human"}]}`, 3},
		{"orchestrator blocker only", `{"blocker_evidence":[{"owner":"orchestrator"}]}`, 3},
		{"absent evidence", `{}`, 3},
		{"empty evidence", `{"blocker_evidence":[]}`, 3},
		{"missing owner", `{"blocker_evidence":[{}]}`, 3},
		{"malformed metadata", `{"blocker_evidence":[{"owner":"instance"}]`, 3},
		{"malformed evidence", `{"blocker_evidence":"instance"}`, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			attempts := append([]store.WorkAttempt{}, base...)
			attempts[0].TerminalState = store.WorkAttemptTerminalSuccess
			attempts[0].WorkerMetadataJSON = tt.metadata
			got := countSessionsWithoutMerge(attempts, time.Time{}, time.Time{})
			if got.Sessions != tt.count || got.exhausted() != (tt.count == 3) {
				t.Fatalf("sessions = %d, exhausted = %v; want %d, %v", got.Sessions, got.exhausted(), tt.count, tt.count == 3)
			}
		})
	}
	for _, class := range []string{"backend_startup_failure", "backend_startup_timeout", "transport_error", "backend_protocol_error", "service_restart", "workspace_preparation", "tracker_unavailable", "forge_unavailable"} {
		t.Run(class+" does not consume allowance", func(t *testing.T) {
			attempts := append([]store.WorkAttempt{}, base...)
			attempts[2].ErrorClass = class
			if got := countSessionsWithoutMerge(attempts, time.Time{}, time.Time{}); got.Sessions != 2 || got.exhausted() {
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
	for _, tt := range []struct {
		name                           string
		kind                           string
		interrupted, optout, noBlocked bool
		want                           string
	}{
		{name: "command gate", kind: gate.KindCommand, want: "Blocked"},
		{name: "interrupted command gate", kind: gate.KindCommand, interrupted: true, want: "Blocked"},
		{name: "human review gate", kind: gate.KindHumanReview, want: "Human Review"},
		{name: "explicit opt out", kind: gate.KindCommand, optout: true, want: "Human Review"},
		{name: "no blocked lane", kind: gate.KindCommand, noBlocked: true, want: "Human Review"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "In Progress"}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
			cfg := laneMutationTestConfig()
			cfg.AutoPromote.Gate.Kind = tt.kind
			cfg.AutoPromote.Gate.RequireAutomatedReview = new(false)
			cfg.AutoPromote.OptoutLabel = "manual-review"
			if tt.optout {
				issue.Labels = []string{"manual-review"}
			}
			if tt.noBlocked {
				cfg.ObservedStates = []string{"Human Review"}
			}
			tracker.refreshed = issue
			db, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			state := newState(cfg)
			note := "## Why this stalled\nThree sessions kept failing the same CI check.\n\n## What is blocking\n- [Failing check](https://github.com/owner/repo/pull/2/checks) needs a fix.\n\n## Options\n- Fix the failing check manually.\n- Close the PR and reduce the scope."
			attempt := store.WorkAttempt{ID: 4, WorkerType: runpkg.RunModeTriage, WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"attempt_allowance_triage": note})}
			if tt.interrupted {
				attempt.WorkerMetadataJSON = "{}"
			}
			for range 2 {
				if err := orch.publishAttemptTriage(t.Context(), &state, issue, attempt, now); err != nil {
					t.Fatal(err)
				}
				issue.State = tt.want
			}
			if len(tracker.comments) != 1 {
				t.Fatalf("comments = %d, want exactly one", len(tracker.comments))
			}
			body := strings.Split(tracker.comments[0].body, "\n\n<!--")[0]
			if !tt.interrupted && body != note {
				t.Fatalf("triage note changed: %s", body)
			}
			if !validAttemptTriageNote(body) {
				t.Fatalf("invalid note: %s", body)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != tt.want {
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

func TestAttemptAllowanceTriageQuotesFinalAssistantMessages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "In Progress"}
	tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
	cfg := laneMutationTestConfig()
	db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
	orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: id, Mode: runpkg.RunModeTriage}
	longMessage := strings.Repeat("waiting for an audit verdict ", 40)
	prior := []store.WorkAttempt{
		{ID: 11, WorkerType: "agent", TerminalState: store.WorkAttemptTerminalNoProgress, WorkerMetadataJSON: marshalWorkAttemptJSON(orch.finalAssistantMessageMetadata(runpkg.RunResult{FinalMessage: "No audit verdict; there is no PR."}))},
		{ID: 12, WorkerType: "agent", TerminalState: store.WorkAttemptTerminalNoProgress, WorkerMetadataJSON: marshalWorkAttemptJSON(orch.finalAssistantMessageMetadata(runpkg.RunResult{FinalMessage: longMessage}))},
		{ID: 13, WorkerType: "agent", TerminalState: store.WorkAttemptTerminalNoProgress, WorkerMetadataJSON: marshalWorkAttemptJSON(orch.finalAssistantMessageMetadata(runpkg.RunResult{FinalMessage: "The issue remains incomplete."}))},
	}
	contextJSON, err := attemptTriageContext(issue, attemptAllowance{Sessions: 3, Attempts: prior})
	if err != nil {
		t.Fatal(err)
	}
	note := "## Why this stalled\nWorkers stopped before opening a PR.\n\n## What is blocking\n- [Issue](https://github.com/owner/repo/issues/1) needs implementation.\n\n## Options\n- Resume implementation manually.\n- Narrow the scope."
	orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Request: runpkg.RunRequest{TriageContext: contextJSON}, CompletedAt: now, Result: runpkg.RunResult{TurnStarted: true, Output: note}})
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v", tracker.comments)
	}
	comment := tracker.comments[0].body
	for _, want := range []string{"Session 1 (attempt 11)", "No audit verdict; there is no PR.", "Session 2 (attempt 12)", "Session 3 (attempt 13)", "The issue remains incomplete.", "[truncated]"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("triage comment missing %q: %s", want, comment)
		}
	}
	if strings.Contains(comment, longMessage) {
		t.Fatal("triage comment included untruncated assistant message")
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
		issue    connector.Issue
		phase    string
		message  string
	}{
		{name: "third code session permitted", sessions: 2, wantMode: runpkg.RunModeImplement},
		{name: "fourth code session replaced by one triage", sessions: 3, wantMode: runpkg.RunModeTriage},
		{name: "infra failure leaves a session", sessions: 3, infra: true, wantMode: runpkg.RunModeImplement},
		{name: "reported question-ending sequence", sessions: 3, wantMode: runpkg.RunModeImplement, phase: "waiting", message: "waiting for a human reply on the original issue"},
		{name: "third conflicted session repairs", sessions: 2, wantMode: runpkg.RunModeMerge, issue: connector.Issue{PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}}},
		{name: "In Progress conflicted sessions reach triage", sessions: 3, wantMode: runpkg.RunModeTriage, issue: connector.Issue{State: "In Progress", PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}}},
		{name: "reported conflicted-session sequence", sessions: 3, wantMode: runpkg.RunModeTriage, issue: connector.Issue{PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}}},
		{name: "conflicted PR with human action", sessions: 3, wantMode: runpkg.RunModeMerge, issue: connector.Issue{PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}, WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "check hardware"}}},
		{name: "reported human hardware wait sequence", sessions: 3, wantMode: runpkg.RunModeImplement, issue: connector.Issue{WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "check hardware"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := tt.issue
			issue.ID = "stalled"
			issue.Identifier = "owner/repo#2595"
			issue.URL = "https://github.com/owner/repo/issues/2595"
			if issue.State == "" {
				issue.State = "Rework"
			}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now.Add(-time.Hour))
			selector := newLaneMutationTestOrchestrator(cfg, nil, db, db, now)
			selectionState := newState(cfg)
			mode := selector.dispatchMode(t.Context(), &selectionState, issue)
			metadata := marshalWorkAttemptJSON(map[string]any{dispatchLoopStartMetadataKey: newDispatchLoopStartRecord(issue, mode), "run_mode": mode})
			for i := range tt.sessions {
				if i > 0 {
					var err error
					id, err = db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: issue.State, AttemptNumber: i + 1, StartedAt: now.Add(-time.Duration(10-i) * time.Minute)})
					if err != nil {
						t.Fatal(err)
					}
				}
				class := ""
				if tt.infra && i == tt.sessions-1 {
					class = "service_restart"
				}
				terminal := store.WorkAttemptTerminalSuccess
				if tt.wantMode == runpkg.RunModeTriage {
					terminal = store.WorkAttemptTerminalFailure
					class = "runner_error"
				}
				if issue.PullRequest != nil {
					class = "no_progress"
				}
				if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(-time.Duration(9-i) * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: terminal, ErrorClass: class, WorkerMetadataJSON: metadata, Phase: tt.phase, StatusMessage: tt.message}); err != nil {
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
				if len(tracker.updates) != 0 {
					t.Fatalf("external wait moved lane: %+v", tracker.updates)
				}
				return
			}
			if len(result.Request.AgentTools) != 0 || result.Request.AgentToolHandler != nil || !strings.Contains(result.Request.TriageContext, "PriorSessions") {
				t.Fatalf("triage request = %#v", result.Request)
			}
			orch.handleRunResult(t.Context(), &state, result)
			if len(tracker.comments) != 1 || len(tracker.updates) != 1 || tracker.updates[0].state != blockedStatusState {
				t.Fatalf("tracker writes: comments=%#v updates=%#v", tracker.comments, tracker.updates)
			}
			issue.State = blockedStatusState
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
			state.Running[issue.ID] = running
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Result: runpkg.RunResult{Output: fallbackAttemptTriageNote(issue, "test")}})
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

func TestAttemptAllowanceTriageInfrastructureFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		failure error
		started bool
	}{
		{"workspace", fmt.Errorf("%w: cannot create directory", runpkg.ErrWorkspacePreparation), false},
		{"startup", backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: backendcapacity.StartupFailureKind}, errors.New("backend exited")), false},
		{"transport", io.ErrUnexpectedEOF, false},
		{"protocol", errors.New("codex turn/start: JSON-RPC -32600 invalid request"), false},
		{"in-turn transport", io.ErrUnexpectedEOF, true},
		{"in-turn protocol", &codex.ResponseError{Request: "turn/start", Code: -32600, Message: "invalid request"}, true},
		{"in-turn overload", backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload}, errors.New("overloaded")), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := connector.Issue{ID: "stalled", Identifier: "owner/repo#2595", URL: "https://github.com/owner/repo/issues/2595", State: "Rework"}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now.Add(-time.Hour))
			for i := range 3 {
				if i > 0 {
					var err error
					id, err = db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: "Rework", AttemptNumber: i + 1, StartedAt: now.Add(-time.Duration(10-i) * time.Minute)})
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(-time.Duration(9-i) * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
					t.Fatal(err)
				}
			}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			orch.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
			orch.runResults = make(chan runpkg.Completion, 1)
			state := newState(cfg)
			if !orch.dispatchIssue(t.Context(), &state, issue, 4, now, "") {
				t.Fatal("triage not dispatched")
			}
			var completion runpkg.Completion
			select {
			case completion = <-orch.runResults:
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not complete")
			}
			completion.Err = tt.failure
			if details, ok := codex.ClassifyCapacityError(tt.failure, nil, now); ok {
				completion.Err = backendcapacity.NewError(backendcapacity.Scope{}, details, tt.failure)
			}
			completion.Result = runpkg.RunResult{TurnStarted: tt.started}
			if tt.started {
				completion.Result.Tokens.TotalTokens = 1
			}
			writesBefore := len(tracker.updates)
			orch.handleRunResult(t.Context(), &state, completion)
			if len(tracker.comments) != 0 || len(tracker.updates) != writesBefore || len(state.Blocked) != 0 {
				t.Fatalf("infrastructure failure produced issue writes: comments=%+v updates=%+v blocked=%+v", tracker.comments, tracker.updates, state.Blocked)
			}
			allowance, err := orch.issueAttemptAllowance(t.Context(), issue)
			if err != nil || allowance.Sessions != 3 || allowance.Triage != nil {
				t.Fatalf("allowance after infrastructure failure=%+v, err=%v", allowance, err)
			}
			// Simulate recovered instance admission with the same durable attempt log.
			restarted := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now.Add(time.Minute))
			restarted.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
			restarted.runResults = make(chan runpkg.Completion, 1)
			recovered := newState(cfg)
			if !restarted.dispatchIssue(t.Context(), &recovered, issue, 5, now.Add(time.Minute), "") {
				t.Fatal("recovered instance could not dispatch triage")
			}
			select {
			case completion = <-restarted.runResults:
			case <-time.After(5 * time.Second):
				t.Fatal("recovered runner did not complete")
			}
			if completion.Request.Mode != runpkg.RunModeTriage {
				t.Fatalf("mode = %s", completion.Request.Mode)
			}
			restarted.handleRunResult(t.Context(), &recovered, completion)
			if len(tracker.comments) != 1 || !validAttemptTriageNote(strings.Split(tracker.comments[0].body, "## Final assistant messages")[0]) {
				t.Fatalf("comments = %+v", tracker.comments)
			}
		})
	}
}

func TestAttemptAllowanceTriageFallbackCompletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		result runpkg.RunResult
		err    error
		detail string
	}{
		{name: "invalid output", result: runpkg.RunResult{TurnStarted: true, Output: "bad format"}, detail: "invalid or missing output"},
		{name: "tool refused", result: runpkg.RunResult{TurnStarted: true}, err: runpkg.ErrSecurityAuditToolUse, detail: runpkg.ErrSecurityAuditToolUse.Error()},
		{name: "interrupted turn", result: runpkg.RunResult{TurnStarted: true}, err: context.Canceled, detail: context.Canceled.Error()},
		{name: "budget admission", result: runpkg.RunResult{BudgetRefusal: &runpkg.BudgetRefusal{Code: string(budget.ReasonPerDayMaxUSD), Message: "daily budget exhausted"}}, detail: "daily budget exhausted"},
		{name: "projection exceeded", result: runpkg.RunResult{TurnStarted: true}, err: &runpkg.SessionBudgetProjectionError{ProjectedCostUSD: 0.1, ObservedCostUSD: 0.2}, detail: "projected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "Rework"}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			state := newState(cfg)
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: id, Mode: runpkg.RunModeTriage}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Result: tt.result, Err: tt.err})
			if len(tracker.comments) != 1 || len(tracker.updates) != 1 || tracker.updates[0].state != blockedStatusState {
				t.Fatalf("writes: comments=%+v updates=%+v", tracker.comments, tracker.updates)
			}
			note := strings.Split(tracker.comments[0].body, "<!--")[0]
			if !validAttemptTriageNote(note) || !strings.Contains(note, tt.detail) {
				t.Fatalf("note = %s", note)
			}
			if len(state.Retry) != 0 || len(state.Blocked) != 0 {
				t.Fatalf("terminal triage scheduled more work: retry=%+v blocked=%+v", state.Retry, state.Blocked)
			}
		})
	}
}

func TestAttemptAllowanceLiveHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, ci, mergeable, want, passState            string
		newHead, disabled, preserve                     bool
		threads                                         []connector.PullRequestReviewThread
		unavailable                                     string
		wantErr                                         bool
		merged, validator, audit, auditRunning, pending bool
	}{
		{name: "merge discovered during hydration", merged: true, ci: "green", mergeable: "clean", want: "Done"},
		{name: "validator pending", validator: true, pending: true, ci: "green", mergeable: "clean"},
		{name: "audit missing defers to merging", audit: true, ci: "green", mergeable: "clean", want: "Merging"},
		{name: "audit missing for non-merging destination", audit: true, pending: true, ci: "green", mergeable: "clean", passState: "Done"},
		{name: "audit running", audit: true, auditRunning: true, pending: true, ci: "green", mergeable: "clean"},
		{name: "green replacement head promotes", newHead: true, ci: "green", mergeable: "clean", want: "Merging"},
		{name: "disabled promotion parks", disabled: true, ci: "green", mergeable: "clean", want: "Blocked"},
		{name: "observed lane is preserved", preserve: true, ci: "green", mergeable: "clean", want: ""},
		{name: "green head promotes", ci: "green", mergeable: "clean", want: "Merging"},
		{name: "failing head parks", ci: "failure", mergeable: "blocked", want: "Blocked"},
		{name: "conflicting head parks", ci: "green", mergeable: "dirty", want: "Blocked"},
		{name: "unresolved thread parks", ci: "green", mergeable: "clean", threads: []connector.PullRequestReviewThread{{Body: "thread"}}, want: "Blocked"},
		{name: "unavailable evidence waits", unavailable: "checks_unavailable", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 14, 21, 55, 0, 0, time.UTC)
			issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "Rework", PullRequest: &connector.PullRequest{Number: 2, State: "open", CIStatus: "failure", HeadSHA: "live"}}
			live := cloneIssue(issue)
			live.PullRequest = &connector.PullRequest{Number: 2, URL: "https://github.com/owner/repo/pull/2", State: "open", HeadSHA: "live", CIStatus: tt.ci, MergeableState: tt.mergeable, CodexReviewState: "COMMENTED", UnresolvedReviewThreads: tt.threads, HydrationUnavailableReason: tt.unavailable, Checks: []connector.PullRequestCheck{{ID: 42, Name: "Smoke", Status: "completed", Conclusion: tt.ci}}}
			if tt.merged {
				live.PullRequest.State = "merged"
			}
			live.PullRequest.BaseSHA = "base"
			if tt.newHead {
				live.PullRequest.HeadSHA = "replacement"
			}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: live}}
			cfg := laneMutationTestConfig()
			cfg.AutoPromote.Enabled = !tt.disabled
			if tt.passState != "" {
				cfg.AutoPromote.PassState = tt.passState
			}
			cfg.AutoPromote.Gate.Validator.Enabled = tt.validator
			cfg.AutoPromote.Gate.SecurityAudit.Enabled = tt.audit
			cfg.ServiceIdentity = "test-service"
			db, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			orch.securityAuditStore = db
			orch.securityAuditRuns = make(map[string]struct{})
			if tt.auditRunning {
				orch.securityAuditRuns[orch.securityAuditIdentity(live).cacheKey] = struct{}{}
			}
			t.Cleanup(orch.securityAuditWG.Wait)
			state := newState(cfg)
			attempt := store.WorkAttempt{ID: 4, WorkerType: runpkg.RunModeTriage, WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"attempt_allowance_triage": fallbackAttemptTriageNote(issue, "Smoke failed earlier"), "attempt_allowance_preserve_lane": tt.preserve})}
			err := orch.publishAttemptTriage(t.Context(), &state, issue, attempt, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tt.wantErr {
				if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
					t.Fatal("unavailable evidence published or moved issue")
				}
				return
			}
			if tt.pending {
				if tt.audit && !tt.auditRunning {
					orch.securityAuditWG.Wait()
					if _, err := db.LatestSecurityAuditRun(t.Context(), orch.securityAuditIdentity(live).key); err != nil {
						t.Fatalf("missing audit was not started: %v", err)
					}
				}
				if tt.validator {
					if _, _, ready := orch.validatorStageResult(t.Context(), live); !ready {
						t.Fatal("missing validator was not started")
					}
				}
				if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
					t.Fatalf("pending stage published or moved issue: %#v %#v", tracker.updates, tracker.comments)
				}
				return
			}
			if tt.preserve {
				if len(tracker.updates) != 0 {
					t.Fatalf("preserved lane changed: %#v", tracker.updates)
				}
				return
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != tt.want {
				t.Fatalf("updates = %#v, want %s", tracker.updates, tt.want)
			}
			// Replay the durable triage after the lane transition, as on restart.
			issue.State = tt.want
			tracker.hydrated.State = tt.want
			if err := orch.publishAttemptTriage(t.Context(), &state, issue, attempt, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(tracker.updates) != 1 {
				t.Fatalf("replay moved lane again: %#v", tracker.updates)
			}
			if tt.want == "Merging" || tt.want == "Done" {
				for _, comment := range tracker.comments {
					if strings.Contains(comment.body, "Smoke failed earlier") {
						t.Fatal("published stale triage")
					}
				}
			} else {
				if len(tracker.comments) != 1 {
					t.Fatalf("comments = %d", len(tracker.comments))
				}
				for _, want := range []string{"live", "Smoke", tt.ci, "2026-09-14T21:55:00Z", "42"} {
					if !strings.Contains(tracker.comments[0].body, want) {
						t.Errorf("triage missing %q: %s", want, tracker.comments[0].body)
					}
				}
			}
		})
	}
}

func TestAttemptAllowanceOperatorMove(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, from, lane string
		origin           provenance.Origin
		want             int
	}{
		{"human rework", "Human Review", "Rework", provenance.OriginHuman, 0},
		{"human todo", "Human Review", "Todo", provenance.OriginHuman, 0},
		{"human merging", "Human Review", "Merging", provenance.OriginHuman, 0},
		{"unknown tracker actor", "Human Review", "Rework", provenance.OriginUnknown, 0},
		{"detent move", "Human Review", "Rework", provenance.OriginDetent, 3},
		{"human from merging", "Merging", "Rework", provenance.OriginHuman, 0},
		{"human from blocked", "Blocked", "Rework", provenance.OriginHuman, 0},
		{"tracker from blocked", "Blocked", "Rework", provenance.OriginUnknown, 0},
		{"detent from merging", "Merging", "Rework", provenance.OriginDetent, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := connector.Issue{ID: "reset", Identifier: "owner/repo#2692", State: "Human Review"}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now.Add(-time.Hour))
			for i := range 3 {
				if i > 0 {
					var err error
					id, err = db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: "Rework", StartedAt: now.Add(-time.Duration(10-i) * time.Minute)})
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(-time.Duration(9-i) * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
					t.Fatal(err)
				}
			}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			triageAt := now.Add(-time.Minute)
			triageID, err := db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: runpkg.RunModeTriage, Lane: "Rework", StartedAt: triageAt})
			if err != nil {
				t.Fatal(err)
			}
			metadata := marshalWorkAttemptJSON(map[string]any{"attempt_allowance_triage": fallbackAttemptTriageNote(issue, "disk exhaustion, PR conflicting")})
			if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: triageID, CompletedAt: triageAt.Add(time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess, WorkerMetadataJSON: metadata}); err != nil {
				t.Fatal(err)
			}
			issue.State = "Rework"
			parked := newState(cfg)
			if err := orch.publishAttemptTriage(t.Context(), &parked, issue, store.WorkAttempt{ID: triageID, WorkerMetadataJSON: metadata}, triageAt.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			issue.State = tt.from
			orch.recordLaneTransition(t.Context(), issue, tt.lane, now, "operator_move", workflowLaneMetadata{Provenance: provenance.Attribution{Origin: tt.origin}})
			issue.State = tt.lane
			got, err := orch.issueAttemptAllowance(t.Context(), issue)
			if err != nil || got.Sessions != tt.want {
				t.Fatalf("sessions=%d want %d, error=%v", got.Sessions, tt.want, err)
			}

			if tt.want != 0 {
				return
			}
			// A fresh runtime must honor the durable operator decision.
			orch = newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			if tt.lane != "Merging" {
				tracker.refreshed, tracker.hydrated = issue, issue
				orch.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
				orch.runResults = make(chan runpkg.Completion, 1)
				state := newState(cfg)
				if !orch.dispatchIssue(t.Context(), &state, issue, 4, now, "") {
					t.Fatal("operator move did not permit dispatch")
				}
				select {
				case result := <-orch.runResults:
					if result.Request.Mode != runpkg.RunModeImplement {
						t.Fatalf("mode=%s", result.Request.Mode)
					}
					if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: state.Running[issue.ID].WorkAttemptID, CompletedAt: now.Add(2 * time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("runner did not complete")
				}
			}
			// Three subsequent sessions exhaust the new window too.
			remaining := 3
			if tt.lane != "Merging" {
				remaining--
			}
			for i := range remaining {
				id, err := db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: tt.lane, StartedAt: now.Add(time.Duration(i+1) * time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(time.Duration(i+1)*time.Minute + time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
					t.Fatal(err)
				}
			}
			got, err = orch.issueAttemptAllowance(t.Context(), issue)
			if err != nil || got.Sessions != 3 || !got.exhausted() || got.Triage != nil {
				t.Fatalf("new window=%+v error=%v", got, err)
			}

			issue.State = "Rework"
			tracker.refreshed, tracker.hydrated = issue, issue
			orch.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
			orch.runResults = make(chan runpkg.Completion, 1)
			state := newState(cfg)
			if !orch.dispatchIssue(t.Context(), &state, issue, 7, now.Add(10*time.Minute), "") {
				t.Fatal("new window triage refused")
			}
			select {
			case result := <-orch.runResults:
				if result.Request.Mode != runpkg.RunModeTriage {
					t.Fatalf("mode=%s", result.Request.Mode)
				}
				orch.handleRunResult(t.Context(), &state, result)
			case <-time.After(5 * time.Second):
				t.Fatal("triage did not complete")
			}
			if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != blockedStatusState {
				t.Fatalf("did not park again: %+v", tracker.updates)
			}
		})
	}
}

func TestAllowanceOperatorMoveBoundary(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	for _, tt := range []struct {
		name, from, to, status, metadata string
		reset                            bool
	}{
		{"human", "Human Review", "Rework", "entered", `{"provenance":{"origin":"human"}}`, true},
		{"legacy detent", "Human Review", "Rework", "entered", `{"provenance":{"origin":"detent"}}`, false},
		{"instance initiator", "Human Review", "Rework", "entered", `{"provenance":{"origin":"external_automation","initiator":"detent_instance"}}`, false},
		{"unattributed", "Human Review", "Todo", "entered", `{}`, true},
		{"other source", "Rework", "Todo", "entered", `{}`, true},
		{"same lane", "Human Review", "Human Review", "entered", `{}`, false},
		{"exit event", "Human Review", "Rework", "exited", `{}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := []store.WorkflowPhaseEvent{{PhaseType: store.WorkflowPhaseTypeLane, PreviousPhaseName: tt.from, PhaseName: tt.to, Status: tt.status, MetadataJSON: tt.metadata, StartedAt: now}}
			got := lastAllowanceOperatorMoveAt(events)
			if got.Equal(now) != tt.reset {
				t.Fatalf("reset=%v want %v", got, tt.reset)
			}
		})
	}
}

type allowanceResetConnector struct {
	attemptTriageConnector
	edits int
}

func (c *allowanceResetConnector) UpdateIssueComment(_ context.Context, _, id, body string) error {
	c.edits++
	for i := range c.refreshed.Comments {
		if c.refreshed.Comments[i].ID == id {
			c.refreshed.Comments[i].Body = body
		}
	}
	return nil
}
func TestAllowanceResetAnnotation(t *testing.T) {
	t.Parallel()
	for _, posted := range []bool{false, true} {
		t.Run(fmt.Sprintf("posted=%v", posted), func(t *testing.T) {
			at := time.Date(2026, 9, 14, 21, 47, 47, 0, time.UTC)
			tracker := &allowanceResetConnector{}
			if posted {
				tracker.refreshed.Comments = []connector.IssueComment{{ID: "note", Body: "triage evidence\n<!-- detent-attempt-triage:4 -->"}}
			}
			orch := &Orchestrator{connector: tracker}
			for range 2 {
				if err := orch.annotateAllowanceReset(t.Context(), connector.Issue{ID: "issue"}, 4, at); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if posted {
				want = 1
			}
			if tracker.edits != want {
				t.Fatalf("edits=%d want %d", tracker.edits, want)
			}
			if posted && !strings.Contains(tracker.refreshed.Comments[0].Body, "allowance reset by operator move at 2026-09-14T21:47:47Z") {
				t.Fatal("missing reset explanation")
			}
		})
	}
}

func TestAttemptAllowanceExternalWaits(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		attempt store.WorkAttempt
		want    int
	}{
		{name: "three question endings", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalSuccess, Phase: "waiting", StatusMessage: "waiting for a human reply on the original issue"}},
		{name: "three conflicted sessions", attempt: store.WorkAttempt{ErrorClass: "no_progress", WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{dispatchLoopStartMetadataKey: newDispatchLoopStartRecord(connector.Issue{PullRequest: &connector.PullRequest{MergeableState: "dirty"}}, runpkg.RunModeImplement)})}, want: 3},
		{name: "historical external wait remains excluded", attempt: store.WorkAttempt{WorkerMetadataJSON: `{"dispatch_loop_start":{"allowance_external_wait":true}}`}},
		{name: "three failed sessions", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalFailure, ErrorClass: "runner_error"}, want: 3},
		{name: "unrelated wait still counts", attempt: store.WorkAttempt{Phase: "waiting", StatusMessage: "waiting for tests"}, want: 3},
		{name: "failed question session counts", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalFailure, Phase: "waiting", StatusMessage: "waiting for a human reply on the original issue"}, want: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			attempts := make([]store.WorkAttempt, 3)
			for i := range attempts {
				attempts[i] = tt.attempt
				attempts[i].WorkerType = "agent"
				attempts[i].ID = int64(i + 1)
			}
			got := countSessionsWithoutMerge(attempts, time.Time{}, time.Time{})
			if got.Sessions != tt.want || got.exhausted() != (tt.want == 3) {
				t.Fatalf("sessions=%d exhausted=%v", got.Sessions, got.exhausted())
			}
		})
	}
}

func TestAttemptAllowanceExternalEvidencePersistence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		issue    connector.Issue
		excluded bool
	}{
		{name: "conflicted PR", issue: connector.Issue{PullRequest: &connector.PullRequest{MergeableState: "dirty"}}},
		{name: "conflicted PR with human action", issue: connector.Issue{PullRequest: &connector.PullRequest{MergeableState: "dirty"}, WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "check hardware"}}, excluded: true},
		{name: "human blocker", issue: connector.Issue{WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{{Owner: workpad.BlockerOwnerHuman, Reason: "check hardware"}}}}, excluded: true},
		{name: "orchestrator blocker", issue: connector.Issue{WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{{Owner: workpad.BlockerOwnerOrchestrator, Reason: "dependency"}}}}},
		{name: "resolved human blocker", issue: connector.Issue{WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusComplete, Blockers: []workpad.Blocker{{Owner: workpad.BlockerOwnerHuman, Reason: "check hardware"}}}}},
		{name: "clean PR", issue: connector.Issue{PullRequest: &connector.PullRequest{MergeableState: "clean"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start := newDispatchLoopStartRecord(tt.issue, runpkg.RunModeImplement)
			// Provider progress and completion refresh must not erase dispatch evidence.
			running := Running{Mode: runpkg.RunModeImplement, DispatchLoopStart: start}
			running.DispatchLoopStart = dispatchLoopStartRecordFromSnapshot(running, runpkg.DispatchLoopStartSnapshot{})
			for _, metadata := range []string{
				marshalWorkAttemptJSON(map[string]any{dispatchLoopStartMetadataKey: start}),
				runningWorkAttemptMetadataJSON(running, nil),
				runningWorkAttemptMetadataJSON(Running{Mode: runpkg.RunModeImplement, Issue: tt.issue}, nil),
			} {
				got := countSessionsWithoutMerge([]store.WorkAttempt{{WorkerType: "agent", WorkerMetadataJSON: metadata}}, time.Time{}, time.Time{})
				if (got.Sessions == 0) != tt.excluded {
					t.Fatalf("sessions=%d metadata=%s", got.Sessions, metadata)
				}
			}
		})
	}
}

func TestAttemptAllowanceExternalWaitRestart(t *testing.T) {
	t.Parallel()
	for _, human := range []bool{false, true} {
		t.Run(fmt.Sprintf("human=%v", human), func(t *testing.T) {
			now := time.Now().UTC()
			issue := connector.Issue{ID: "external-wait", Identifier: "owner/repo#2789", State: "Rework"}
			cfg := laneMutationTestConfig()
			db, seed := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now.Add(-time.Hour))
			if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: seed, CompletedAt: now.Add(-time.Hour), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalFailure, ErrorClass: "service_restart"}); err != nil {
				t.Fatal(err)
			}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			state := newState(cfg)
			blocked := issue
			blocked.PullRequest = &connector.PullRequest{MergeableState: "dirty"}
			if human {
				blocked.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{{Owner: workpad.BlockerOwnerHuman, Reason: "hardware check"}}}
			}
			for i := range 3 {
				at := now.Add(time.Duration(i) * time.Minute)
				start := newDispatchLoopStartRecord(blocked, runpkg.RunModeImplement)
				id, ok := orch.startDurableWorkAttempt(t.Context(), &state, blocked, i+1, at, "", runpkg.RunModeImplement, start)
				if !ok {
					t.Fatal("start failed")
				}
				got, err := orch.issueAttemptAllowance(t.Context(), issue)
				want := i + 1
				if human {
					want = 0
				}
				if err != nil || got.Sessions != want {
					t.Fatalf("active allowance=%+v err=%v", got, err)
				}
				// A refreshed clean issue preserves both chargeable sessions and human waits.
				running := Running{Issue: issue, Mode: runpkg.RunModeImplement, WorkAttemptID: id, StartedAt: at, DispatchLoopStart: start}
				if !orch.completeDurableWorkAttemptWithMetadata(t.Context(), &state, running, at.Add(time.Second), store.WorkAttemptTerminalSuccess, "", "", "completed", "", nil) {
					t.Fatal("completion failed")
				}
			}
			restarted := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			got, err := restarted.issueAttemptAllowance(t.Context(), issue)
			want := 3
			if human {
				want = 0
			}
			if err != nil || got.Sessions != want || got.exhausted() != !human {
				t.Fatalf("restart allowance=%+v err=%v", got, err)
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("allowance accounting wrote lane: %+v", tracker.updates)
			}
		})
	}
}
