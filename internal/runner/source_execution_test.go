package runner

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type existingReviewTestWorkspace struct {
	*fakeWorkspaceBackend
	info     workspace.Info
	attempt  workspace.AttemptWorkspace
	used     bool
	released bool
}

func (w *existingReviewTestWorkspace) Existing(issue workspace.Issue) (workspace.Info, error) {
	w.attempt = issue.AttemptWorkspace
	return w.info, nil
}

func (w *existingReviewTestWorkspace) Use(context.Context, workspace.Issue) (func(), error) {
	w.used = true
	return func() { w.released = true }, nil
}

func (w *existingReviewTestWorkspace) ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error) {
	return "review-head", "review-tree", nil
}

func (*existingReviewTestWorkspace) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func TestReviewContinuationReusesOnlyTheExactHeldPhase(t *testing.T) {
	for _, test := range []struct {
		name     string
		mismatch func(*tracker.NativePhaseReceipt)
		opened   bool
	}{
		{name: "current attempt and fence", opened: true},
		{name: "lost fence", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.FencingToken-- }},
		{name: "different attempt", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.AttemptID = "attempt_other" }},
		{name: "different version head", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.HeadSHA = "other-head" }},
		{name: "different checkout tree", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.TreeSHA = "other-tree" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := workspace.AttemptWorkspace{Organization: "org_fixture", Project: "prj_fixture", Attempt: "attempt_11111111111111111111111111111111"}
			lease := tracker.NativeLease{ID: "lease_fixture", FencingToken: 14, PolicyID: "policy_fixture"}
			phase := tracker.NativePhaseReceipt{Phase: "project_gate", AttemptID: attempt.Attempt, PolicyID: lease.PolicyID,
				Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken},
				Branch:   "detent/item", HeadSHA: "review-head", TreeSHA: "review-tree"}
			if test.mismatch != nil {
				test.mismatch(&phase)
			}
			backend := &existingReviewTestWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{}, info: workspace.Info{PublicationBranch: "detent/item"}}
			req := RunRequest{Issue: connector.Issue{ID: "wi_fixture"}, AttemptWorkspace: attempt,
				Execution: &testExecution{recovery: tracker.NativeRecovery{Lease: lease}}, RetainItemWorkspace: true,
				phaseReceipts: true, phaseContinuation: &tracker.NativePhaseContinuation{LastPushed: &phase},
				NativeReviewContinuation: &NativeReviewContinuation{Source: tracker.NativeChangeReference{VersionID: "version_fixture", HeadSHA: "review-head"}}}
			runner := &Runner{}
			retained, release := runner.existingReviewWorkspace(t.Context(), req, backend)
			if (retained != nil) != test.opened || test.opened && (backend.attempt != attempt || backend.released) || backend.created {
				t.Fatalf("retained=%t, attempt=%+v, released=%t, created=%t", retained != nil, backend.attempt, backend.released, backend.created)
			}
			release()
			if backend.used != backend.released {
				t.Fatal("worktree use was not released")
			}
		})
	}
}

func TestNativeSourceRework(t *testing.T) {
	t.Parallel()
	complete := "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name    string
		change  func(*NativeChange)
		message string
		want    bool
	}{
		{name: "validator rework", want: true},
		{name: "implementation rework", want: true, change: func(c *NativeChange) { c.ImplementationReview, c.Validator = c.Validator, nil }},
		{name: "audit rework", want: true, change: func(c *NativeChange) {
			c.Validator = nil
			c.SecurityAudit = &tracker.SecurityAuditReview{VersionID: "v2", Verdict: "rework", Run: securityaudit.Run{HeadSHA: "head2"}}
		}},
		{name: "wait", change: func(c *NativeChange) { c.Validator.Verdict = "wait" }},
		{name: "infrastructure", change: func(c *NativeChange) { c.Error = "provider unavailable" }},
		{name: "review infrastructure", change: func(c *NativeChange) { c.ImplementationReviewError = "provider unavailable" }},
		{name: "source custody", change: func(c *NativeChange) { c.VersionError = "source unavailable" }},
		{name: "stale policy", change: func(c *NativeChange) { c.VersionError, c.VersionCode = "stale policy", "policy_mismatch" }},
		{name: "landing failure", change: func(c *NativeChange) { c.Landing = &NativeLanding{GateFailed: true} }},
		{name: "blocked", message: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Supply access\n```"},
		{name: "human action", message: strings.Replace(complete, "human_action: null", "human_action: Supply access", 1)},
		{name: "unfinished", message: strings.Replace(complete, "status: complete", "status: in_progress", 1)},
		{name: "older head", change: func(c *NativeChange) { c.Validator.HeadSHA = "head1" }},
		{name: "older version", change: func(c *NativeChange) { c.Validator.VersionID = "v1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			change := &NativeChange{Changed: true, VersionID: "v2", HeadSHA: "head2", Validator: &gate.ValidatorResult{VersionID: "v2", HeadSHA: "head2", Verdict: "rework", Summary: "Repair the defect"}}
			if test.change != nil {
				test.change(change)
			}
			message := complete
			if test.message != "" {
				message = test.message
			}
			if got := nativeSourceRework(change, message); (got != "") != test.want {
				t.Fatalf("feedback=%q want repair=%v", got, test.want)
			}
		})
	}
}

func TestSourceTurnBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		updates       []AgentUpdate
		wantRemaining int
	}{
		{name: "provider without turn notifications", wantRemaining: 2},
		{name: "two distinct turns", updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "one"}, {Type: AgentUpdateTurnStarted, TurnID: "two"}}, wantRemaining: 1},
		{name: "repeated notification counts once", updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "one"}, {Type: AgentUpdateTurnStarted, TurnID: "one"}}, wantRemaining: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := &sourceTurnBudget{remaining: 3}
			ctx := context.WithValue(t.Context(), sourceTurnBudgetKey{}, budget)
			backend := &fakeCodexClient{updates: test.updates}
			if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{MaxTurns: 20}, nil, nil, nil); err != nil || cleanupErr != nil {
				t.Fatal(errors.Join(err, cleanupErr))
			}
			if backend.request.MaxTurns != 3 || budget.remaining != test.wantRemaining {
				t.Fatalf("per-call budget reset: max=%d remaining=%d", backend.request.MaxTurns, budget.remaining)
			}
			backend.updates = nil
			for budget.remaining > 0 {
				remaining := budget.remaining
				if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{MaxTurns: 20}, nil, nil, nil); err != nil || cleanupErr != nil {
					t.Fatal(errors.Join(err, cleanupErr))
				}
				if backend.request.MaxTurns != remaining {
					t.Fatalf("remaining budget=%d provider limit=%d", remaining, backend.request.MaxTurns)
				}
			}
			calls := backend.calls
			if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{}, nil, nil, nil); !errors.Is(err, ErrSessionTurnLimitExceeded) || cleanupErr != nil || backend.calls != calls {
				t.Fatalf("exhausted budget started another provider call: %v calls=%d", err, backend.calls)
			}
		})
	}
}

type phaseCheckoutExecution struct {
	*testExecution
	PhaseReceiptExecution
	continuation tracker.NativePhaseContinuation
}

func (e *phaseCheckoutExecution) PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error) {
	return &e.continuation, nil
}

type phaseCheckoutWorkspace struct {
	*fakeWorkspaceBackend
	itemPhaseWorkspace
	issue workspace.Issue
	stop  error
}

func (w *phaseCheckoutWorkspace) Create(_ context.Context, issue workspace.Issue) (workspace.Info, error) {
	w.issue = issue
	return workspace.Info{}, w.stop
}

func TestReplacementImplementationSelectsPushedPhaseOverOlderChange(t *testing.T) {
	head := strings.Repeat("a", 40)
	change := &tracker.NativeChangeReference{VersionID: "older-version", HeadSHA: strings.Repeat("b", 40)}
	receipt := tracker.NativePhaseReceipt{Mutation: tracker.Mutation{LeaseID: "old-lease"}, HeadSHA: head, Phase: "implement"}
	execution := &phaseCheckoutExecution{
		testExecution: &testExecution{recovery: tracker.NativeRecovery{
			Lease: tracker.NativeLease{ID: "replacement-lease"}, Change: change, SourceVersionID: change.VersionID,
		}},
		continuation: tracker.NativePhaseContinuation{LastPushed: &receipt, NextPhase: "project_gate"},
	}
	backend := &phaseCheckoutWorkspace{stop: errors.New("stop at checkout")}
	r, err := NewRunner(Dependencies{
		Workflow:  config.Workflow{Config: config.Default(), Prompt: "Complete the issue"},
		Workspace: backend, AgentBackend: &releaseDeliveryAgent{}, ProcessSource: emptyProcessSource, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Execution: execution, Issue: connector.Issue{ID: "1", Identifier: "EX-1"}})
	if !errors.Is(err, backend.stop) {
		t.Fatalf("run did not reach checkout: %v", err)
	}
	if backend.issue.PhaseSource == nil || backend.issue.PhaseSource.HeadSHA != head {
		t.Fatalf("checkout phase = %+v, want %s", backend.issue.PhaseSource, head)
	}
	if !backend.issue.VerifyPhaseBranch || backend.issue.PhaseChangeHeadSHA != change.HeadSHA {
		t.Fatalf("checkout must verify the branch and the current Change ancestry: %+v", backend.issue)
	}
}
