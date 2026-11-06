package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNoChangeCompletionRequiresCurrentSourceGateReceipts(t *testing.T) {
	for _, test := range []struct {
		name    string
		merging bool
		missing string
		reject  string
		stale   bool
		audit   bool
		want    string
	}{
		{name: "accepted evidence closes without a Change", want: "Done"},
		{name: "empty Change from merging completes without landing", merging: true, want: "Done"},
		{name: "stale claim reverified on current base", stale: true, want: "Done"},
		{name: "review rejection returns to implementation", reject: "review", want: "Rework"},
		{name: "validation rejection returns to implementation", reject: "validator", want: "Rework"},
		{name: "missing review cannot close", missing: "review", want: "Todo"},
		{name: "missing validation cannot close", missing: "validator", want: "Todo"},
		{name: "stale validator receipt cannot close", missing: "current source", want: "Todo"},
		{name: "configured audit cannot be bypassed", audit: true, want: "Todo"},
		{name: "command gate cannot be bypassed", missing: "command", want: "Todo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			head := strings.Repeat("b", 40)
			n := &tracker.NativeNoChangeCompletion{ReportedSHA: head, VerifiedSHA: head, Evidence: "go test ./internal/example passed; attachment att_example", PolicyID: "policy_example", Repository: "https://github.com/example-org/example-project", Validation: &gate.CommandResult{Command: "go test ./...", HeadSHA: head, TreeSHA: "tree", ExitCode: 0}}
			if test.stale {
				n.ReportedSHA = strings.Repeat("a", 40)
			}
			review := gate.ValidatorResult{VersionID: n.VersionID(), Repository: "https://github.com/example-org/example-project", BaseSHA: head, HeadSHA: head, SessionID: 11, Submitted: true, Verdict: "pass", Score: 10}
			validator := review
			validator.SessionID = 12
			change := &runpkg.NativeChange{NoChange: n, BaseSHA: head, HeadSHA: head, ImplementationReview: &review, Validator: &validator}
			switch test.missing {
			case "review":
				change.ImplementationReview = nil
			case "validator":
				change.Validator = nil
			case "current source":
				validator.HeadSHA = n.ReportedSHA + "old"
			case "command":
				n.Validation = nil
			}
			if test.reject == "review" {
				review.Verdict, review.Summary = "rework", "Add the missing acceptance regression"
			}
			if test.reject == "validator" {
				validator.Verdict, validator.Summary = "rework", "Add the missing acceptance regression"
			}
			issue := connector.Issue{ID: "wi_example", Identifier: "example#1", State: "In Progress"}
			mode := runpkg.RunModeImplement
			if test.merging {
				issue.State, mode = "Merging", runpkg.RunModeMerge
			}
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			conn := &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: []connector.WorkflowState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done", "Rework", "Todo"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"Done", "Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "Done", Terminal: true},
				{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Rework", "Todo"}},
			}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{HumanReview: new(false), ReworkState: "Rework", Gate: gate.Config{Kind: gate.KindCommand, Run: "go test ./...", AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}, SecurityAudit: gate.SecurityAuditConfig{Enabled: test.audit}}}})
			attempts := &recordingWorkAttemptStore{}
			o := &Orchestrator{cfg: cfg, connector: conn, workAttempts: attempts}
			state := newState(cfg)
			now := time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)
			running := Running{Issue: issue, Attempt: 4, WorkAttemptID: 42, Generation: 7, Mode: mode, StartedAt: now.Add(-time.Minute), Tokens: TokenTotals{TotalTokens: 10}}
			state.Running[issue.ID] = running
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			message := "```detent-status\nschema: 1\nstatus: no_change_needed\nfields:\n  completion_verified_sha: " + n.ReportedSHA + "\n  completion_evidence: " + n.Evidence + "\n```"
			o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: mode}, Result: runpkg.RunResult{FinalState: FinalStateCompleted, TurnStarted: true, NativeChange: change, FinalMessage: message, DiffStats: DiffStats{Status: "clean", HeadSHA: head}}})
			if len(tick.updates) != 1 || tick.updates[0].state != test.want {
				t.Fatalf("lane updates = %+v; want %s; retry=%+v deferred=%+v", tick.updates, test.want, state.Retry, state.deferredCompletions)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].ErrorClass == terminalAttemptWithoutWorkProductReason {
				t.Fatalf("no-change outcome classified without work product: %+v", attempts.completions)
			}
			if test.want == "Done" {
				if len(state.Retry) != 0 || len(tick.comments) != 1 || !strings.Contains(tick.comments[0].body, n.Evidence) || !strings.Contains(tick.comments[0].body, head) {
					t.Fatalf("closure lost acceptance evidence: comments=%+v retry=%+v", tick.comments, state.Retry)
				}
			} else {
				retry, ok := state.Retry[issue.ID]
				if !ok || retry.Attempt != 5 {
					t.Fatalf("retry accounting changed: %+v", state.Retry)
				}
				if test.reject != "" && !strings.Contains(retry.Error, "missing acceptance regression") {
					t.Fatalf("rejection feedback lost: %+v", retry)
				}
			}
		})
	}
}
