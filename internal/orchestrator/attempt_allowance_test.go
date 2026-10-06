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
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type attemptTriageConnector struct{ implementProgressConnector }

func (c *attemptTriageConnector) CreateComment(ctx context.Context, id, body string) error {
	c.refreshed.Comments = append(c.refreshed.Comments, connector.IssueComment{Body: body})
	return c.implementProgressConnector.CreateComment(ctx, id, body)
}

func TestAttemptAllowanceTriagePublication(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name                           string
		kind                           string
		interrupted, optout, noBlocked bool
		humanReview                    *bool
		want                           string
	}{
		{name: "command gate", kind: gate.KindCommand, want: "Blocked"},
		{name: "interrupted command gate", kind: gate.KindCommand, interrupted: true, want: "Blocked"},
		{name: "human review gate", kind: gate.KindHumanReview, want: "Human Review"},
		{name: "explicit opt out", kind: gate.KindCommand, optout: true, want: "Human Review"},
		{name: "review disabled human gate", kind: gate.KindHumanReview, humanReview: new(false), want: "Blocked"},
		{name: "review disabled opt out", kind: gate.KindCommand, optout: true, humanReview: new(false), want: "Blocked"},
		{name: "no blocked lane", kind: gate.KindCommand, noBlocked: true, want: "Human Review"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{ID: "issue", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", State: "In Progress"}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue}}
			cfg := laneMutationTestConfig()
			cfg.AutoPromote.Gate.Kind = tt.kind
			cfg.AutoPromote.HumanReview = tt.humanReview
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

type attemptTriageRunner struct{}

func (attemptTriageRunner) Run(_ context.Context, req runpkg.RunRequest) (runpkg.RunResult, error) {
	return runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: fallbackAttemptTriageNote(req.Issue, "test evidence")}, nil
}

func TestDispatchRetainsConfiguredModeAfterPriorSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
		{name: "fourth code session retains implementation", sessions: 3, wantMode: runpkg.RunModeImplement},
		{name: "fourth current clean head retains implementation", sessions: 3, wantMode: runpkg.RunModeImplement, issue: connector.Issue{PullRequest: &connector.PullRequest{Number: 42, State: "open", HeadSHA: "fourth-head", MergeableState: "clean"}}},
		{name: "infra failure leaves a session", sessions: 3, infra: true, wantMode: runpkg.RunModeImplement},
		{name: "reported question-ending sequence", sessions: 3, wantMode: runpkg.RunModeImplement, phase: "waiting", message: "waiting for a human reply on the original issue"},
		{name: "third conflicted session repairs", sessions: 2, wantMode: runpkg.RunModeImplement, issue: connector.Issue{PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}}},
		{name: "In Progress conflicted sessions retain merge repair", sessions: 3, wantMode: runpkg.RunModeMerge, issue: connector.Issue{State: "In Progress", PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}}},
		{name: "reported conflicted-session sequence", sessions: 3, wantMode: runpkg.RunModeImplement, issue: connector.Issue{PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}}},
		{name: "conflicted PR with human action", sessions: 3, wantMode: runpkg.RunModeImplement, issue: connector.Issue{PullRequest: &connector.PullRequest{State: "open", MergeableState: "dirty"}, WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "check hardware"}}},
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
				if tt.sessions >= 3 {
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
			if result.Request.TriageContext != "" || len(tracker.updates) != 0 || len(tracker.comments) != 0 {
				t.Fatalf("unexpected retired triage: request=%+v updates=%+v comments=%+v", result.Request, tracker.updates, tracker.comments)
			}
		})
	}
}

func TestAttemptAllowancePreservesOperatorCompletionLane(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
			completion.Request.Mode = runpkg.RunModeTriage
			running := state.Running[issue.ID]
			running.Mode = runpkg.RunModeTriage
			state.Running[issue.ID] = running
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
			if completion.Request.Mode != runpkg.RunModeImplement {
				t.Fatalf("mode = %s", completion.Request.Mode)
			}
		})
	}
}

func TestAttemptAllowanceTriageFallbackCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name, ci, mergeable, want, passState            string
		newHead, disabled, preserve                     bool
		threads                                         []connector.PullRequestReviewThread
		requiredChecks                                  []connector.PullRequestCheck
		unavailable                                     string
		receipt                                         string
		wantErr                                         bool
		merged, validator, audit, auditRunning, pending bool
	}{
		{name: "merge discovered during hydration", merged: true, ci: "green", mergeable: "clean", want: "Done"},
		{name: "fresh corrected merged receipt", receipt: "valid", ci: "green", mergeable: "clean", want: "Done"},
		{name: "merged receipt preserves observed lane", receipt: "valid", preserve: true, ci: "green", mergeable: "clean"},
		{name: "wrong receipt integration branch remains held", receipt: "wrong branch", ci: "green", mergeable: "clean", want: "Blocked"},
		{name: "missing merged reference remains held", receipt: "missing reference", ci: "green", mergeable: "clean", want: "Blocked"},
		{name: "current human action remains held", receipt: "human action", ci: "green", mergeable: "clean", want: "Blocked"},
		{name: "validator pending", validator: true, pending: true, ci: "green", mergeable: "clean"},
		{name: "audit missing starts before merging", audit: true, pending: true, ci: "green", mergeable: "clean"},
		{name: "audit missing for non-merging destination", audit: true, pending: true, ci: "green", mergeable: "clean", passState: "Done"},
		{name: "audit running", audit: true, auditRunning: true, pending: true, ci: "green", mergeable: "clean"},
		{name: "green replacement head promotes", newHead: true, ci: "green", mergeable: "clean", want: "Merging"},
		{name: "disabled promotion parks", disabled: true, ci: "green", mergeable: "clean", want: "Blocked"},
		{name: "observed lane is preserved", preserve: true, ci: "green", mergeable: "clean", want: ""},
		{name: "green head promotes", ci: "green", mergeable: "clean", want: "Merging"},
		{name: "pending CI waits", ci: "pending", mergeable: "blocked", pending: true},
		{name: "pending CI and missing audit wait", ci: "pending", mergeable: "blocked", audit: true, pending: true},
		{name: "missing required check parks despite running CI", ci: "pending", mergeable: "blocked", requiredChecks: []connector.PullRequestCheck{{Name: "Required", Status: "missing", Conclusion: "missing"}}, want: "Blocked"},
		{name: "queued required check waits", ci: "pending", mergeable: "blocked", requiredChecks: []connector.PullRequestCheck{{Name: "Required", Status: "queued"}}, pending: true},
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
			if tt.ci == "pending" {
				live.PullRequest.Checks[0].Status = "in_progress"
				live.PullRequest.RunningChecks = []string{"Smoke"}
			}
			live.PullRequest.RequiredCheckFailures = tt.requiredChecks
			if tt.merged {
				live.PullRequest.State = "merged"
			}
			live.PullRequest.BaseSHA = "base"
			if tt.newHead {
				live.PullRequest.HeadSHA = "replacement"
			}
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: live}}
			if tt.receipt != "" {
				tracker.hydrated.PullRequest.State = "closed"
				body := strings.ReplaceAll(mergedCompletionWorkpadBody(), "example/repo", "owner/repo")
				if tt.receipt == "wrong branch" {
					body = strings.ReplaceAll(body, "origin/main", "origin/feature")
				}
				if tt.receipt == "human action" {
					body = strings.ReplaceAll(body, "human_action: null", "human_action: approve release")
				}
				tracker.refreshed.Comments = []connector.IssueComment{{Body: body, CreatedAt: &now, UpdatedAt: &now, AuthorAuthorized: true}}
				merged := &connector.PullRequest{Number: 12, URL: "https://github.com/owner/repo/pull/12", State: "merged", HeadSHA: "merged-head", BaseRef: "main", CIStatus: "success"}
				tracker.resolvedBlockers = []connector.Issue{{Identifier: "owner/repo#12", PullRequest: merged}, {Identifier: "owner/repo#2", PullRequest: tracker.hydrated.PullRequest}}
				if tt.receipt == "missing reference" {
					tracker.resolvedBlockers = nil
				}
			}
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
				if tt.audit && !tt.auditRunning && tt.ci != "pending" {
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
