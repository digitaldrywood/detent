package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type receiptReviewExecution struct {
	*implementationReviewExecution
	steps            []tracker.NativeStepReceipt
	publicationError error
	usage            []tracker.NativeUsage
	publicationCheck func()
}

type receiptReviewSessions struct {
	*fakeSessionStore
	beforeFinish    func()
	fail            string
	committed       bool
	failed          bool
	durable         map[string]any
	reconciliations int
}

func (s *receiptReviewSessions) FinishSession(ctx context.Context, id int64, finish store.SessionFinish) error {
	if s.beforeFinish != nil {
		s.beforeFinish()
	}
	return s.write("finish", finish, func() error { return s.fakeSessionStore.FinishSession(ctx, id, finish) })
}

func (s *receiptReviewSessions) write(operation string, value any, write func() error) error {
	if s.durable == nil {
		s.durable = map[string]any{}
	}
	if previous, ok := s.durable[operation]; ok {
		previousRaw, err := json.Marshal(previous)
		if err != nil {
			return err
		}
		valueRaw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if !bytes.Equal(previousRaw, valueRaw) {
			return fmt.Errorf("changed immutable %s", operation)
		}
		return nil
	}
	fail := s.fail == operation && !s.failed
	if fail {
		s.failed = true
	}
	if !fail || s.committed {
		if err := write(); err != nil {
			return err
		}
		s.durable[operation] = value
	}
	if fail {
		return fmt.Errorf("%s response unavailable", operation)
	}
	return nil
}

func (s *receiptReviewSessions) RecordUsageEvent(ctx context.Context, usage store.UsageEvent) (int64, error) {
	err := s.write("usage", usage, func() error { _, err := s.fakeSessionStore.RecordUsageEvent(ctx, usage); return err })
	return 1, err
}

func (s *receiptReviewSessions) RecordWorkflowPhaseEvent(ctx context.Context, phase store.WorkflowPhaseEvent) (int64, error) {
	err := s.write("phase", phase, func() error { _, err := s.fakeSessionStore.RecordWorkflowPhaseEvent(ctx, phase); return err })
	return 1, err
}

func (s *receiptReviewSessions) ReconcileSessionFinalization(ctx context.Context, id int64, finalization *sessionmodel.Finalization) error {
	s.reconciliations++
	if finalization == nil {
		if len(s.durable) != 3 {
			return errors.New("session finalization incomplete")
		}
		return nil
	}
	if id != 73 || finalization.SessionID != id || finalization.Usage.SessionID != id || finalization.Phase == nil || finalization.Phase.SessionID != id {
		return errors.New("session identity changed")
	}
	if err := s.FinishSession(ctx, id, finalization.Finish); err != nil {
		return err
	}
	if _, err := s.RecordUsageEvent(ctx, finalization.Usage); err != nil {
		return err
	}
	_, err := s.RecordWorkflowPhaseEvent(ctx, *finalization.Phase)
	return err
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
	if e.publicationCheck != nil {
		e.publicationCheck()
	}
	e.reviewResult = result
	return e.publicationError
}

func (e *receiptReviewExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	if e.publicationCheck != nil {
		e.publicationCheck()
	}
	e.validatorResult = result
	return e.publicationError
}

func TestNativeReviewPublicationRetry(t *testing.T) {
	for _, step := range []string{"security_audit", "review", "validator"} {
		t.Run(step, func(t *testing.T) {
			for _, failure := range []struct {
				name, operation string
				committed       bool
			}{
				{name: "publication"},
				{name: "finish rejected", operation: "finish"},
				{name: "finish committed response lost", operation: "finish", committed: true},
				{name: "usage rejected", operation: "usage"},
				{name: "usage committed response lost", operation: "usage", committed: true},
				{name: "phase rejected", operation: "phase"},
				{name: "phase committed response lost", operation: "phase", committed: true},
			} {
				if step == "security_audit" && failure.operation != "" {
					continue
				}
				t.Run(failure.name, func(t *testing.T) {
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
					sessions := &receiptReviewSessions{fakeSessionStore: &fakeSessionStore{sessionID: 73}, fail: failure.operation, committed: failure.committed}
					r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: cfg}, Workspace: worktree, AgentBackends: map[string]AgentBackend{"reviewer": agent}, SecurityAuditRoot: t.TempDir(), Store: sessions, Pricing: budget.PricingTable{"test-model": {USDPerInputToken: 0.001, USDPerOutputToken: 0.002}}})
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
					if step != "security_audit" {
						sessions.beforeFinish = func() {
							if len(first.steps) != 1 || first.steps[0].Step != step || first.steps[0].Validator == nil {
								t.Errorf("session cleanup started without the verified %s receipt", step)
								return
							}
							receipt := first.steps[0]
							if receipt.CompletedAt.IsZero() || receipt.Validator.SessionID != 73 || receipt.Validator.VersionID != "version_example" || receipt.Validator.HeadSHA != "head" || receipt.Source.HeadSHA != "head" || receipt.Change.VersionID != "version_example" {
								t.Errorf("session cleanup preceded complete source-bound evidence: %+v", receipt)
							}
						}
					}
					observeUsage = func() {
						if len(first.usage) == 0 || first.usage[0].CostEstimate <= 0 {
							t.Fatalf("running provider turn has no priced usage: %+v", first.usage)
						}
					}
					publications := 0
					checkPublication := func() {
						publications++
						if len(sessions.durable) != 3 {
							t.Errorf("publication preceded durable finalization: %v", sessions.durable)
						}
						if failure.operation != "" && sessions.reconciliations < 2 {
							t.Error("publication bypassed finalization reconciliation")
						}
					}
					first.publicationCheck = checkPublication
					if failure.operation == "" {
						first.publicationError = errors.New("publication unavailable")
					}
					if err := invoke(first); err == nil {
						t.Fatal("publication failure succeeded")
					} else if failure.operation == "" {
						if terminal := nativeTerminalFailure(err, time.Now(), false); terminal.Stage != step+"_publication" {
							t.Fatalf("failure = %+v", terminal)
						}
					}
					if failure.operation != "" && publications != 0 {
						t.Fatal("failed finalization published")
					}
					encoded, err := json.Marshal(first.steps)
					if err != nil {
						t.Fatal(err)
					}
					second := newExecution()
					second.publicationCheck = checkPublication
					if err := json.Unmarshal(encoded, &second.steps); err != nil {
						t.Fatal(err)
					}
					if err := invoke(second); err != nil || agent.calls != 1 || len(second.usage) != 0 {
						t.Fatalf("retry error=%v provider turns=%d", err, agent.calls)
					}
					if sessions.finishCalls != 1 || sessions.usageCalls != 1 || sessions.usage.SessionID != 73 {
						t.Fatalf("session accounting changed: finish=%d usage=%d session=%d", sessions.finishCalls, sessions.usageCalls, sessions.usage.SessionID)
					}
					if len(sessions.durable) != 3 {
						t.Fatalf("finalization incomplete: %v", sessions.durable)
					}
				})
			}
		})
	}
}
