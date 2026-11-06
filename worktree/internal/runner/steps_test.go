package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type receiptReviewExecution struct {
	*implementationReviewExecution
	steps            []tracker.NativeStepReceipt
	publicationError error
	usage            []tracker.NativeUsage
}

func (e *receiptReviewExecution) RecordUsage(_ context.Context, usage tracker.NativeUsage) error {
	e.usage = append(e.usage, usage)
	return nil
}

func (e *receiptReviewExecution) RecordStep(_ context.Context, receipt tracker.NativeStepReceipt) error {
	e.steps = append(e.steps, receipt)
	return nil
}

func (e *receiptReviewExecution) Step(_ context.Context, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	for _, receipt := range e.steps {
		if receipt.Matches(binding) {
			return receipt, nil
		}
	}
	return tracker.NativeStepReceipt{}, nil
}

func (e *receiptReviewExecution) RecordSecurityAudit(_ context.Context, result tracker.SecurityAuditReview) error {
	e.auditResult = result
	return e.publicationError
}

func (e *receiptReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	e.reviewResult = result
	return e.publicationError
}

func (e *receiptReviewExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	e.validatorResult = result
	return e.publicationError
}

func TestNativeReviewPublicationRetry(t *testing.T) {
	for _, step := range []string{"security_audit", "review", "validator"} {
		t.Run(step, func(t *testing.T) {
			output := `{"verdict":"pass","score":1,"summary":"No findings","findings":[],"criteria_evidence":[]}`
			if step == "security_audit" {
				output = `{"verdict":"pass","summary":"No findings","findings":[]}`
			}
			var observeUsage func()
			agent := &completedTurnBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: time.Now()}}, {Type: AgentUpdateTokenUsage, Model: "test-model", Tokens: AgentTokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}}, {Type: AgentUpdateMessageDelta, Delta: output}}}, afterTurn: func() { observeUsage() }}
			agent.result.AuthenticationMode = "api_key"
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Gate.SecurityAudit.Enabled = step == "security_audit"
			cfg.Gate.AutomatedReview = gate.AutomatedReviewOff
			if step == "review" {
				cfg.Gate.AutomatedReview = gate.AutomatedReviewRequired
			}
			cfg.Gate.Validator.Enabled = step == "validator"
			cfg.Agents.ModelSelection.Enabled = new(false)
			cfg.Agents.Backends = []config.AgentBackend{{ID: "reviewer", Kind: "codex", Protocol: "app-server", Command: "test-agent"}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "review", Backend: "reviewer", Model: "test-model", Default: true}}
			worktree := &implementationReviewWorkspace{attachmentValidatorWorkspace: attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, heads: []string{"head"}}}}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: cfg}, Workspace: worktree, AgentBackends: map[string]AgentBackend{"reviewer": agent}, SecurityAuditRoot: t.TempDir(), Store: &fakeSessionStore{sessionID: 73}, Pricing: budget.PricingTable{"test-model": {USDPerInputToken: 0.001, USDPerOutputToken: 0.002}}})
			if err != nil {
				t.Fatal(err)
			}
			newExecution := func() *receiptReviewExecution {
				events := []string{}
				e := &receiptReviewExecution{implementationReviewExecution: &implementationReviewExecution{events: &events}}
				e.recovery.Issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example"}}
				return e
			}
			invoke := func(e *receiptReviewExecution) error {
				req := RunRequest{Execution: e, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1"}}
				switch step {
				case "security_audit":
					_, err := r.auditNativeChange(t.Context(), req)
					return err
				case "review":
					_, err := r.reviewNativeChange(t.Context(), req)
					return err
				default:
					return r.validateNativeChange(t.Context(), req)
				}
			}
			first := newExecution()
			observeUsage = func() {
				if len(first.usage) == 0 || first.usage[0].CostEstimate <= 0 {
					t.Fatalf("running provider turn has no priced usage: %+v", first.usage)
				}
			}
			first.publicationError = errors.New("publication unavailable")
			if err := invoke(first); err == nil {
				t.Fatal("publication failure succeeded")
			} else if failure := nativeTerminalFailure(err, time.Now(), false); failure.Stage != step+"_publication" {
				t.Fatalf("failure = %+v", failure)
			}
			encoded, err := json.Marshal(first.steps)
			if err != nil {
				t.Fatal(err)
			}
			second := newExecution()
			if err := json.Unmarshal(encoded, &second.steps); err != nil {
				t.Fatal(err)
			}
			if err := invoke(second); err != nil || agent.calls != 1 || len(second.usage) != 0 {
				t.Fatalf("retry error=%v provider turns=%d", err, agent.calls)
			}
		})
	}
}
