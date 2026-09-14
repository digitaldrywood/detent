package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerTriageIsReadOnly(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		updates []AgentUpdate
		wantErr bool
	}{
		{"note", []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: "triage note"}}, false},
		{"tool refused", []AgentUpdate{{Type: AgentUpdateToolStarted}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeCodexClient{updates: tt.updates}
			workspace := &fakeWorkspaceBackend{}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{}}, Workspace: workspace,
				AgentBackend: backend, Store: &fakeSessionStore{sessionID: 2595}, SecurityAuditRoot: t.TempDir(),
				Now: time.Now,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(t.Context(), RunRequest{
				Issue: connector.Issue{ID: "stalled", Identifier: "owner/repo#1", State: "Rework"}, Mode: RunModeTriage,
				TriageContext: "CI failed; three sessions; review thread https://example.com/review",
				AgentTools:    []AgentTool{{Name: "file_machine_issue"}},
				AgentToolHandler: func(_ context.Context, _ AgentToolCall) (AgentToolResult, error) {
					t.Error("triage exposed worker mutation tool")
					return AgentToolResult{}, nil
				},
			})
			if tt.wantErr {
				if !errors.Is(err, ErrSecurityAuditToolUse) {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || result.Output != "triage note" {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			request := backend.request
			if !request.ReadOnly || request.SupplementalTools || len(request.ExtraWritableRoots) != 0 || request.Resume != (AgentResume{}) {
				t.Fatalf("triage restrictions = %#v", request)
			}
			if !strings.Contains(request.Prompt, "https://example.com/review") || request.MaxTurns != 1 {
				t.Fatalf("triage input/turn bound = %#v", request)
			}
			if backend.calls != 1 {
				t.Fatalf("backend calls = %d", backend.calls)
			}
		})
	}
}

type triageExecution struct {
	testExecution
	identity tracker.NativeExecutionIdentity
	startErr error
}

func (e *triageExecution) Start(_ context.Context, identity tracker.NativeExecutionIdentity) error {
	e.started = true
	e.identity = identity
	return e.startErr
}

type triageCatalogBackend struct{ fakeCodexClient }

func (*triageCatalogBackend) ListModels(context.Context, AgentProcessRequest) ([]AgentModel, error) {
	return selectionCatalog(), nil
}

func TestRunnerTriageNativeAdmission(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		startErr error
	}{
		{name: "reserved model starts execution"},
		{name: "lost reservation refuses backend", startErr: ErrExecutionAuthorityUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.ModelSelection.Enabled = new(false)
			backend := &triageCatalogBackend{}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{}, AgentBackend: backend, SecurityAuditRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			execution := &triageExecution{startErr: tt.startErr}
			req := RunRequest{Mode: RunModeTriage, Issue: connector.Issue{ID: "triage", State: "Rework", ModelOverride: "gpt-5.6-sol", Description: "```detent-agent\nschema: 1\nmodel: gpt-6-astra\n```"}}
			report := providercapacity.Report{Backend: "codex", Models: []string{"gpt-5.6-sol"}}
			req.ProviderReports = []providercapacity.Report{report}
			capacity, err := r.DispatchCapacity(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			req.Execution = &capacityTestExecution{Execution: execution, reservation: &providercapacity.Reservation{Requirement: capacity, Report: report}}
			_, err = r.Run(t.Context(), req)
			if !errors.Is(err, tt.startErr) {
				t.Fatalf("Run error = %v, want %v", err, tt.startErr)
			}
			if !execution.started || execution.identity.Model != capacity.Model || execution.identity.Backend != capacity.Backend || execution.identity.Role != capacity.Role {
				t.Fatalf("execution identity = %+v, reservation = %+v", execution.identity, capacity)
			}
			wantCalls := 1
			if tt.startErr != nil {
				wantCalls = 0
			}
			if backend.calls != wantCalls {
				t.Fatalf("backend calls = %d, want %d", backend.calls, wantCalls)
			}
			if wantCalls > 0 && backend.request.Model != capacity.Model {
				t.Fatalf("turn model = %s, reserved %s", backend.request.Model, capacity.Model)
			}
		})
	}
}

func TestRunnerTriageBudget(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                string
		refusal             budget.ReasonCode
		sessions            int64
		wantProjectionError bool
	}{
		{name: "issue cap refuses dispatch", refusal: budget.ReasonPerIssueMaxUSD},
		{name: "daily cap refuses dispatch", refusal: budget.ReasonPerDayMaxUSD},
		{name: "historical projection stops turn", sessions: 5, wantProjectionError: true},
		{name: "default projection stays advisory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{InputTokens: 11, TotalTokens: 11}}}}
			checker := &fakeBudgetChecker{refusal: budget.Refusal{Code: tt.refusal}, projection: &budget.Projection{CostUSD: 0.10, Estimate: budget.TokenEstimate{InputTokens: 10, TotalTokens: 10, Sessions: tt.sessions}}}
			sessions := &fakeSessionStore{sessionID: 2595}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Budget: config.Budget{BillingMode: config.BillingModeMetered}}}, Workspace: &fakeWorkspaceBackend{}, AgentBackend: backend, Store: sessions, SecurityAuditRoot: t.TempDir(), BudgetChecker: checker, Pricing: budget.PricingTable{"gpt-budget": {USDPerInputToken: 0.01}}})
			if err != nil {
				t.Fatal(err)
			}
			execution := &triageExecution{}
			usageUpdates := 0
			result, err := r.Run(t.Context(), RunRequest{Mode: RunModeTriage, Execution: execution, Issue: connector.Issue{ID: "triage", ModelOverride: "gpt-budget"}, OnUsageUpdate: func(UsageUpdate) error {
				usageUpdates++
				return nil
			}})
			if errors.Is(err, ErrSessionBudgetProjectionExceeded) != tt.wantProjectionError || err != nil && !tt.wantProjectionError {
				t.Fatalf("Run error = %v", err)
			}
			if checker.calls != 1 {
				t.Fatalf("budget checks = %d", checker.calls)
			}
			if tt.refusal != "" {
				if result.BudgetRefusal == nil || result.BudgetRefusal.Code != string(tt.refusal) || backend.calls != 0 || execution.started {
					t.Fatalf("refused result=%+v backend calls=%d started=%v", result, backend.calls, execution.started)
				}
			} else if backend.calls != 1 || usageUpdates == 0 || sessions.usage.ProjectedCostUSD == nil || *sessions.usage.ProjectedCostUSD != 0.10 || sessions.usage.CostUSD <= 0.10 {
				t.Fatalf("backend calls=%d, usage=%+v", backend.calls, sessions.usage)
			}
		})
	}
}

func TestRunnerTriageReportsTurnStart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		started AgentUpdate
	}{
		{"turn notification", AgentUpdate{Type: AgentUpdateTurnStarted}},
		{"primary turn identity", AgentUpdate{Type: AgentUpdateMessageDelta, TurnID: "turn-1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeCodexClient{updates: []AgentUpdate{tt.started, {Type: AgentUpdateToolStarted}}}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}}, Workspace: &fakeWorkspaceBackend{}, AgentBackend: backend, SecurityAuditRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			observedTurns := 0
			result, err := r.Run(t.Context(), RunRequest{Mode: RunModeTriage, Issue: connector.Issue{ID: "triage"}, OnUsageUpdate: func(update UsageUpdate) error { observedTurns = update.TurnCount; return nil }})
			if !errors.Is(err, ErrSecurityAuditToolUse) || !result.TurnStarted || observedTurns != 1 || result.Tokens.TotalTokens != 0 {
				t.Fatalf("result=%+v turns=%d err=%v", result, observedTurns, err)
			}
		})
	}
}

type triageCapacityBackend struct {
	fakeCodexClient
	classifyCalls int
}

func (b *triageCapacityBackend) ClassifyCapacityError(err error, _ *telemetry.RateLimits, _ time.Time) (backendcapacity.Details, bool) {
	b.classifyCalls++
	return backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload}, errors.Is(err, b.err)
}

func TestRunnerTriageClassifiesProviderFailure(t *testing.T) {
	t.Parallel()
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%v", started), func(t *testing.T) {
			backend := &triageCapacityBackend{fakeCodexClient: fakeCodexClient{err: errors.New("provider unavailable")}}
			if started {
				backend.updates = []AgentUpdate{{Type: AgentUpdateTurnStarted}, {Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{TotalTokens: 1, InputTokens: 1}}}
			}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}}, Workspace: &fakeWorkspaceBackend{}, AgentBackend: backend, SecurityAuditRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{Mode: RunModeTriage, Issue: connector.Issue{ID: "triage"}})
			capacityErr, ok := backendcapacity.As(err)
			if !ok || capacityErr.Details.Type != backendcapacity.ErrorTypeTransientOverload || backend.classifyCalls != 1 || result.TurnStarted != started {
				t.Fatalf("result=%+v err=%v classifier calls=%d", result, err, backend.classifyCalls)
			}
		})
	}
}
