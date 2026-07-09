package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/notes"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerRunPreparesWorkspaceRunsCodexAndRecordsSession(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	writeSkill(t, workspacePath, "review.md", "review", "Review code.", "Issue needs code review.")

	startedAt := time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(4 * time.Second)
	modelContextWindow := int64(200000)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{
			Path:   workspacePath,
			Key:    "digitaldrywood_detent_22",
			Branch: "detent/digitaldrywood_detent_22",
		},
		diffStats: []workspace.DiffStat{
			{Files: 1, Added: 2},
			{Files: 2, Added: 5, Removed: 1},
			{Files: 2, Added: 5, Removed: 1},
			{Files: 2, Added: 5, Removed: 1},
		},
	}
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{
			{
				Type:            AgentUpdateMessageDelta,
				ProcessIdentity: "4242",
				ThreadID:        "thread-1",
				TurnID:          "turn-1",
				ItemID:          "item-1",
				Delta:           "hello",
			},
			{
				Type:     AgentUpdateTokenUsage,
				ThreadID: "thread-1",
				TurnID:   "turn-1",
				Model:    "gpt-5-codex-resolved",
				Tokens: AgentTokenUsage{
					InputTokens:           100,
					CachedInputTokens:     40,
					OutputTokens:          25,
					ReasoningOutputTokens: 7,
					TotalTokens:           125,
					ModelContextWindow:    &modelContextWindow,
				},
			},
			{
				Type: AgentUpdateRateLimits,
				RateLimits: &telemetry.RateLimits{
					LimitID:   "codex-primary",
					LimitName: "Codex primary",
					Credits: &telemetry.RateLimitBucket{
						HasCredits: true,
						Balance:    "7.25",
					},
				},
			},
		},
		result: AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1", SessionID: "thread-1-turn-1"},
	}
	sessionStore := &fakeSessionStore{sessionID: 42}
	now := newFakeClock(
		startedAt,
		startedAt.Add(time.Second),
		startedAt.Add(2*time.Second),
		startedAt.Add(3*time.Second),
		completedAt,
		completedAt,
	)
	prNumber := 133

	runner, err := NewRunner(Dependencies{
		ProjectID: "detent",
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{
					Skills: config.Skills{
						Enabled:           true,
						Path:              ".detent/skills",
						MaxSkillsInPrompt: 10,
					},
				},
			},
			Prompt: "Work on {{ issue.identifier }} attempt {{ attempt }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
		Store:        sessionStore,
		Pricing: budget.PricingTable{
			"gpt-5-codex-high": {
				USDPerInputToken:       0.000004,
				USDPerCachedInputToken: 0.000001,
				USDPerOutputToken:      0.00002,
			},
			"gpt-5-codex-resolved": {
				USDPerInputToken:       0.000004,
				USDPerCachedInputToken: 0.000001,
				USDPerOutputToken:      0.00002,
			},
		},
		Now: now.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	var usageUpdates []UsageUpdate
	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-22",
			Identifier:    "digitaldrywood/detent#22",
			Title:         "Add runner",
			URL:           "https://github.com/digitaldrywood/detent/issues/22",
			PRNumber:      &prNumber,
			BranchName:    "detent/digitaldrywood_detent_22",
			ModelOverride: "gpt-5-codex-high",
		},
		Attempt:   2,
		StartedAt: startedAt,
		OnUsageUpdate: func(update UsageUpdate) error {
			usageUpdates = append(usageUpdates, update)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateCompleted)
	}
	if result.Tokens.TotalTokens != 125 || result.Tokens.RuntimeSeconds != 4 {
		t.Fatalf("Tokens = %#v, want total 125 and runtime 4s", result.Tokens)
	}
	if result.Model != "gpt-5-codex-resolved" || result.Tokens.CachedInputTokens != 40 || result.Tokens.ReasoningOutputTokens != 7 {
		t.Fatalf("RunResult telemetry = %#v, want resolved model and cached/reasoning tokens", result)
	}
	if result.Tokens.ModelContextWindow == nil || *result.Tokens.ModelContextWindow != modelContextWindow {
		t.Fatalf("RunResult ModelContextWindow = %#v, want %d", result.Tokens.ModelContextWindow, modelContextWindow)
	}
	if len(usageUpdates) != 4 {
		t.Fatalf("usage updates len = %d, want 4", len(usageUpdates))
	}
	if usageUpdates[0].DetentSessionID != 42 || usageUpdates[0].LastEvent != string(AgentUpdateRuntimeIdentity) {
		t.Fatalf("initial usage update = %#v, want configured route identity", usageUpdates[0])
	}
	if usageUpdates[0].RuntimeIdentity.RequestedModel != (agentidentity.Value{Value: "gpt-5-codex-high", Provenance: agentidentity.ProvenanceConfigured}) {
		t.Fatalf("initial RuntimeIdentity = %#v, want configured requested model", usageUpdates[0].RuntimeIdentity)
	}
	if usageUpdates[1].SessionID != "thread-1-turn-1" || usageUpdates[1].TurnCount != 1 {
		t.Fatalf("second usage update = %#v, want live session and one turn", usageUpdates[1])
	}
	if usageUpdates[1].ProcessIdentity != "4242" {
		t.Fatalf("second usage update ProcessIdentity = %q, want 4242", usageUpdates[1].ProcessIdentity)
	}
	if usageUpdates[1].WorkspacePath != workspacePath {
		t.Fatalf("second usage update WorkspacePath = %q, want %q", usageUpdates[1].WorkspacePath, workspacePath)
	}
	if usageUpdates[1].LastEvent != "agent_message_delta" || usageUpdates[1].LastMessage != "hello" {
		t.Fatalf("second usage update activity = %#v, want agent message", usageUpdates[1])
	}
	if len(usageUpdates[1].RecentEvents) != 2 || usageUpdates[1].RecentEvents[1].Message != "hello" {
		t.Fatalf("second usage update RecentEvents = %#v, want route and agent message", usageUpdates[1].RecentEvents)
	}
	if usageUpdates[1].LastEventAt.IsZero() {
		t.Fatal("second usage update LastEventAt is zero")
	}
	if usageUpdates[1].DiffStats.FilesChanged != 1 || usageUpdates[1].DiffStats.AddedLines != 2 || usageUpdates[1].DiffStats.Status != "ok" {
		t.Fatalf("second usage update DiffStats = %#v, want live diff", usageUpdates[1].DiffStats)
	}
	if usageUpdates[2].TurnCount != 1 || usageUpdates[2].Tokens.TotalTokens != 125 {
		t.Fatalf("third usage update = %#v, want 1 turn and 125 tokens", usageUpdates[2])
	}
	if usageUpdates[2].Tokens.RuntimeSeconds != 3 {
		t.Fatalf("third usage update runtime = %v, want 3", usageUpdates[2].Tokens.RuntimeSeconds)
	}
	if len(usageUpdates[2].RecentEvents) != 3 || usageUpdates[2].RecentEvents[2].Event != "token_usage" || usageUpdates[2].RecentEvents[2].Message != "125 total tokens (100 in, 25 out)" {
		t.Fatalf("third usage update RecentEvents = %#v, want token-specific activity", usageUpdates[2].RecentEvents)
	}
	if usageUpdates[2].DiffStats.FilesChanged != 2 || usageUpdates[2].DiffStats.AddedLines != 5 || usageUpdates[2].DiffStats.RemovedLines != 1 {
		t.Fatalf("third usage update DiffStats = %#v, want refreshed diff", usageUpdates[2].DiffStats)
	}
	if usageUpdates[3].RateLimits == nil || usageUpdates[3].RateLimits.LimitID != "codex-primary" {
		t.Fatalf("fourth usage update RateLimits = %#v, want codex-primary", usageUpdates[3].RateLimits)
	}
	if len(usageUpdates[3].RecentEvents) != 4 || usageUpdates[3].RecentEvents[3].Event != "rate_limits" || usageUpdates[3].RecentEvents[3].Message != "Codex primary rate limits updated" {
		t.Fatalf("fourth usage update RecentEvents = %#v, want rate-limit-specific activity", usageUpdates[3].RecentEvents)
	}
	if usageUpdates[3].DiffStats.FilesChanged != 2 || usageUpdates[3].DiffStats.AddedLines != 5 || usageUpdates[3].DiffStats.RemovedLines != 1 {
		t.Fatalf("fourth usage update DiffStats = %#v, want refreshed diff", usageUpdates[3].DiffStats)
	}
	if result.DiffStats.FilesChanged != 2 || result.DiffStats.AddedLines != 5 || result.DiffStats.RemovedLines != 1 {
		t.Fatalf("DiffStats = %#v, want 2 files, 5 added, 1 removed", result.DiffStats)
	}
	if result.RateLimits == nil || result.RateLimits.LimitID != "codex-primary" {
		t.Fatalf("RateLimits = %#v, want codex-primary", result.RateLimits)
	}
	if result.RateLimits.Credits == nil || !result.RateLimits.Credits.HasCredits || result.RateLimits.Credits.Balance != "7.25" {
		t.Fatalf("RateLimits.Credits = %#v, want available balance 7.25", result.RateLimits.Credits)
	}
	if !workspaceBackend.created || !workspaceBackend.beforeRun || !workspaceBackend.afterRun || !workspaceBackend.diffed {
		t.Fatalf("workspace calls = created:%v before:%v after:%v diff:%v, want all true", workspaceBackend.created, workspaceBackend.beforeRun, workspaceBackend.afterRun, workspaceBackend.diffed)
	}
	if workspaceBackend.createIssue.ProjectID != "detent" ||
		workspaceBackend.createIssue.ID != "issue-22" ||
		workspaceBackend.createIssue.Identifier != "digitaldrywood/detent#22" ||
		workspaceBackend.createIssue.BranchName != "detent/digitaldrywood_detent_22" {
		t.Fatalf("Create() issue = %#v", workspaceBackend.createIssue)
	}
	if workspaceBackend.diffCalls != 3 {
		t.Fatalf("DiffStat calls = %d, want throttled live calls plus final stat", workspaceBackend.diffCalls)
	}
	if codexClient.request.Workspace != workspacePath {
		t.Fatalf("codex workspace = %q, want %q", codexClient.request.Workspace, workspacePath)
	}
	if codexClient.request.Model != "gpt-5-codex-high" {
		t.Fatalf("codex model = %q, want issue override", codexClient.request.Model)
	}
	for _, want := range []string{
		"Work on digitaldrywood/detent#22 attempt 2",
		"## Available skills",
		"review — Issue needs code review.",
	} {
		if !strings.Contains(codexClient.request.Prompt, want) {
			t.Fatalf("codex prompt missing %q:\n%s", want, codexClient.request.Prompt)
		}
	}
	if sessionStore.started.Identifier != "digitaldrywood/detent#22" || sessionStore.started.Model != "" || sessionStore.started.RequestedModel != "gpt-5-codex-high" || sessionStore.started.AgentRole != RoleCode {
		t.Fatalf("SessionStart = %#v, want requested model distinct from unresolved model and code role", sessionStore.started)
	}
	if sessionStore.finished.FinalState != FinalStateCompleted || sessionStore.finished.TotalTokens != 125 || sessionStore.finished.Turns != 1 || sessionStore.finished.Model != "gpt-5-codex-resolved" {
		t.Fatalf("SessionFinish = %#v, want completed session with tokens", sessionStore.finished)
	}
	if sessionStore.finished.ProviderThreadID != "thread-1" || sessionStore.finished.ProviderSessionID != "thread-1-turn-1" {
		t.Fatalf("SessionFinish provider IDs = %#v, want thread-1/thread-1-turn-1", sessionStore.finished)
	}
	if len(sessionStore.identityUpdates) != 1 || sessionStore.identityUpdates[0].ResolvedModel != (agentidentity.Value{Value: "gpt-5-codex-resolved", Provenance: agentidentity.ProvenanceRuntime}) {
		t.Fatalf("identity updates = %#v, want immediate runtime model persistence", sessionStore.identityUpdates)
	}
	if sessionStore.finished.CachedInputTokens != 40 || sessionStore.finished.ReasoningOutputTokens != 7 {
		t.Fatalf("SessionFinish cached/reasoning = %#v, want 40/7", sessionStore.finished)
	}
	if sessionStore.finished.ModelContextWindow == nil || *sessionStore.finished.ModelContextWindow != modelContextWindow {
		t.Fatalf("SessionFinish ModelContextWindow = %#v, want %d", sessionStore.finished.ModelContextWindow, modelContextWindow)
	}
	if sessionStore.usage.ProjectID != "detent" || sessionStore.usage.SessionID != 42 {
		t.Fatalf("UsageEvent identity = %#v, want project detent and session 42", sessionStore.usage)
	}
	if sessionStore.usage.IssueID != "issue-22" || sessionStore.usage.Identifier != "digitaldrywood/detent#22" {
		t.Fatalf("UsageEvent issue = %#v, want issue-22/digitaldrywood/detent#22", sessionStore.usage)
	}
	if sessionStore.usage.Model != "gpt-5-codex-resolved" || sessionStore.usage.TotalTokens != 125 || sessionStore.usage.CachedInputTokens != 40 || sessionStore.usage.ReasoningOutputTokens != 7 {
		t.Fatalf("UsageEvent totals = %#v, want resolved model, total 125, cached 40, reasoning 7", sessionStore.usage)
	}
	if sessionStore.usage.ModelContextWindow == nil || *sessionStore.usage.ModelContextWindow != modelContextWindow {
		t.Fatalf("UsageEvent ModelContextWindow = %#v, want %d", sessionStore.usage.ModelContextWindow, modelContextWindow)
	}
	if math.Abs(sessionStore.usage.CostUSD-0.00078) > 0.000000000001 {
		t.Fatalf("UsageEvent CostUSD = %.12f, want 0.000780000000", sessionStore.usage.CostUSD)
	}
	if sessionStore.usage.PRNumber == nil || *sessionStore.usage.PRNumber != 133 {
		t.Fatalf("UsageEvent PRNumber = %v, want 133", sessionStore.usage.PRNumber)
	}
	if sessionStore.usage.StartedAt != startedAt || sessionStore.usage.FinishedAt != completedAt {
		t.Fatalf("UsageEvent timestamps = %s/%s, want %s/%s", sessionStore.usage.StartedAt, sessionStore.usage.FinishedAt, startedAt, completedAt)
	}
	if sessionStore.usage.Outcome != FinalStateCompleted {
		t.Fatalf("UsageEvent outcome = %q, want %q", sessionStore.usage.Outcome, FinalStateCompleted)
	}
	if sessionStore.phase.ProjectID != "detent" || sessionStore.phase.SessionID != 42 {
		t.Fatalf("WorkflowPhaseEvent identity = %#v, want project detent and session 42", sessionStore.phase)
	}
	if sessionStore.phase.EndpointFamily != "codex" {
		t.Fatalf("WorkflowPhaseEvent EndpointFamily = %q, want codex", sessionStore.phase.EndpointFamily)
	}
	if sessionStore.phase.PhaseType != store.WorkflowPhaseTypeAgentSession || sessionStore.phase.PhaseName != "agent_active" || sessionStore.phase.Status != FinalStateCompleted {
		t.Fatalf("WorkflowPhaseEvent phase = %#v, want completed agent_active session", sessionStore.phase)
	}
	if sessionStore.phase.StartedAt != startedAt || sessionStore.phase.FinishedAt != completedAt || sessionStore.phase.DurationSeconds != 4 {
		t.Fatalf("WorkflowPhaseEvent timing = %#v, want 4s session", sessionStore.phase)
	}
	if sessionStore.phase.Turns != 1 || sessionStore.phase.InputTokens != 100 || sessionStore.phase.CachedInputTokens != 40 || sessionStore.phase.OutputTokens != 25 || sessionStore.phase.ReasoningOutputTokens != 7 || sessionStore.phase.TotalTokens != 125 {
		t.Fatalf("WorkflowPhaseEvent usage = %#v, want turns and token totals", sessionStore.phase)
	}
	if sessionStore.phase.ModelContextWindow == nil || *sessionStore.phase.ModelContextWindow != modelContextWindow {
		t.Fatalf("WorkflowPhaseEvent ModelContextWindow = %#v, want %d", sessionStore.phase.ModelContextWindow, modelContextWindow)
	}
}

func TestConfiguredRuntimeIdentityKeepsClaudeIntentDistinct(t *testing.T) {
	t.Parallel()

	workflow, err := config.ParseWorkflow([]byte(`---
tracker:
  kind: memory
agents:
  backends:
    - id: claude-local
      kind: claude_code
      provider: ollama
      options:
        effort: high
  routes:
    - name: local
      backend: claude-local
      model: fable
      default: true
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	backend := workflow.Config.AgentBackendConfigs()[0]
	identity := configuredRuntimeIdentity(RouteSelection{BackendID: "claude-local", RouteName: "local"}, backend, RoleCode, "fable", time.Time{})
	if identity.Provider != (agentidentity.Value{Value: "ollama", Provenance: agentidentity.ProvenanceConfigured}) {
		t.Fatalf("Provider = %#v, want configured ollama", identity.Provider)
	}
	if identity.RequestedModel != (agentidentity.Value{Value: "fable", Provenance: agentidentity.ProvenanceConfigured}) || identity.ResolvedModel.Known() {
		t.Fatalf("model identity = %#v, want configured request and unresolved runtime model", identity)
	}
	if identity.ReasoningEffort != (agentidentity.Value{Value: "high", Provenance: agentidentity.ProvenanceConfigured}) {
		t.Fatalf("ReasoningEffort = %#v, want configured high", identity.ReasoningEffort)
	}
	_, _, effort := agentTurnIdentityOptions(backend)
	if effort != "high" {
		t.Fatalf("turn effort = %q, want high", effort)
	}
}

func TestRunnerRunRefusesDispatchWhenBudgetExceeded(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	startedAt := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{
			Path:   workspacePath,
			Key:    "digitaldrywood_detent_855",
			Branch: "detent/digitaldrywood_detent_855",
		},
	}
	agentBackend := &fakeCodexClient{
		updates: []AgentUpdate{
			{Type: AgentUpdateMessageDelta, Delta: "should not run"},
		},
	}
	spendStore := &fakeRunnerBudgetSpendStore{
		daily: store.TokenSpend{
			ByModel: []store.ModelTokenSpend{
				{Model: "gpt-budget", InputTokens: 120},
			},
		},
	}
	checker := budget.NewChecker(budget.Config{
		Enabled:         true,
		PerDayMaxUSD:    1.25,
		RefusalCooldown: time.Hour,
	}, spendStore, budget.PricingTable{
		"gpt-budget": {
			USDPerInputToken:  0.01,
			USDPerOutputToken: 0.02,
		},
	})
	estimator := &fakeDispatchEstimator{
		estimate: budget.TokenEstimate{
			InputTokens:  10,
			OutputTokens: 0,
			TotalTokens:  10,
			Sessions:     5,
		},
	}
	sessionStore := &fakeSessionStore{sessionID: 855}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{},
			Prompt: "Work on {{ issue.identifier }}",
		},
		Workspace:         workspaceBackend,
		AgentBackend:      agentBackend,
		Store:             sessionStore,
		BudgetChecker:     checker,
		DispatchEstimator: estimator,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-855",
			Identifier:    "digitaldrywood/detent#855",
			URL:           "https://github.com/digitaldrywood/detent/issues/855",
			BranchName:    "detent/digitaldrywood_detent_855",
			ModelOverride: "gpt-budget",
		},
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.BudgetRefusal == nil {
		t.Fatal("BudgetRefusal = nil, want refusal")
	}
	if result.BudgetRefusal.Code != string(budget.ReasonPerDayMaxUSD) || result.BudgetRefusal.Message != "daily budget exceeded" {
		t.Fatalf("BudgetRefusal = %#v, want daily budget refusal", result.BudgetRefusal)
	}
	if !strings.Contains(result.BudgetRefusal.Comment, "projected dispatch would exceed the daily budget") {
		t.Fatalf("BudgetRefusal.Comment = %q, want refusal comment", result.BudgetRefusal.Comment)
	}
	if agentBackend.calls != 0 {
		t.Fatalf("RunTurn calls = %d, want 0", agentBackend.calls)
	}
	if sessionStore.startCalls != 0 || sessionStore.finishCalls != 0 || sessionStore.usageCalls != 0 {
		t.Fatalf("session store calls = start %d finish %d usage %d, want none", sessionStore.startCalls, sessionStore.finishCalls, sessionStore.usageCalls)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("workspace AfterRun = false, want cleanup after refusal")
	}
	if spendStore.dailyCalls != 1 {
		t.Fatalf("DailyTokenSpend calls = %d, want 1", spendStore.dailyCalls)
	}
	if estimator.model != "gpt-budget" {
		t.Fatalf("estimator model = %q, want gpt-budget", estimator.model)
	}
}

func TestRunnerRunRecordsRuntimeModelUpdate(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-1103", Branch: "detent/issue-1103"},
		diffStats: []workspace.DiffStat{
			{Files: 1, Added: 3},
		},
	}
	agentBackend := &fakeCodexClient{
		updates: []AgentUpdate{
			{Type: AgentUpdateTurnStarted, ThreadID: "thread-1103", TurnID: "turn-1"},
			{Type: AgentUpdateModelUpdated, ThreadID: "thread-1103", TurnID: "turn-1", Model: "gpt-5.6"},
			{
				Type:     AgentUpdateTokenUsage,
				ThreadID: "thread-1103",
				TurnID:   "turn-1",
				Tokens: AgentTokenUsage{
					InputTokens:  100,
					OutputTokens: 10,
					TotalTokens:  110,
				},
			},
		},
		result: AgentTurnResult{ThreadID: "thread-1103", TurnID: "turn-1", SessionID: "thread-1103-turn-1"},
	}
	sessionStore := &fakeSessionStore{sessionID: 1103}
	startedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Prompt: "Work"},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Pricing: budget.PricingTable{
			"gpt-5.6": {
				USDPerInputToken:       0.000006,
				USDPerCachedInputToken: 0.0000006,
				USDPerOutputToken:      0.000036,
			},
		},
		Now: newFakeClock(startedAt, startedAt.Add(time.Second), startedAt.Add(2*time.Second), startedAt.Add(3*time.Second)).Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-1103",
			Identifier: "digitaldrywood/detent#1103",
			Title:      "Resolve runtime model",
		},
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Model != "gpt-5.6" {
		t.Fatalf("RunResult.Model = %q, want runtime model", result.Model)
	}
	if sessionStore.started.Model != "" {
		t.Fatalf("SessionStart.Model = %q, want bare route to start without a pinned model", sessionStore.started.Model)
	}
	if sessionStore.finished.Model != "gpt-5.6" {
		t.Fatalf("SessionFinish.Model = %q, want runtime model", sessionStore.finished.Model)
	}
	if sessionStore.usage.Model != "gpt-5.6" {
		t.Fatalf("UsageEvent.Model = %q, want runtime model", sessionStore.usage.Model)
	}
	if sessionStore.usage.CostUSD == 0 {
		t.Fatal("UsageEvent.CostUSD = 0, want priced runtime model")
	}
}

func TestRunnerRunLogsBudgetRefusalWithDerivedRole(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-903", Branch: "detent/issue-903"},
	}
	agentBackend := &fakeCodexClient{}
	checker := &fakeBudgetChecker{
		refusal: budget.Refusal{
			Code:      budget.ReasonPerDayMaxUSD,
			Message:   "daily budget exceeded",
			RefusedAt: now,
		},
	}

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agents: config.Agents{
					Backends: []config.AgentBackend{{
						ID:       "codex-rework",
						Kind:     "codex",
						Protocol: "app-server",
						Command:  "codex app-server --profile rework",
					}},
					Routes: []config.AgentRoute{{
						Name:    "rework",
						Role:    RoleRework,
						Backend: "codex-rework",
						Model:   "gpt-5-rework",
						Default: true,
					}},
				},
			},
			Prompt: "work {{ issue.identifier }}",
		},
		Workspace:     workspaceBackend,
		AgentBackends: map[string]AgentBackend{"codex-rework": agentBackend},
		BudgetChecker: checker,
		Now:           newFakeClock(now).Now,
		Logger:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-903",
			Identifier: "digitaldrywood/detent#903",
			State:      "Rework",
		},
		Mode:      RunModeImplement,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.BudgetRefusal == nil {
		t.Fatal("BudgetRefusal = nil, want refusal")
	}
	logText := logs.String()
	if !strings.Contains(logText, "worker_budget_refused") {
		t.Fatalf("logs missing worker_budget_refused:\n%s", logText)
	}
	if !strings.Contains(logText, "role=rework") {
		t.Fatalf("budget refusal log = %q, want role=rework", logText)
	}
	if strings.Contains(logText, "worker_budget_refused") && strings.Contains(logText, "role=code") {
		t.Fatalf("budget refusal log = %q, want no code role for rework refusal", logText)
	}
}

func TestRunnerRunLeavesThreadResumeDisabledByDefault(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 2, 16, 0, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-859", Branch: "detent/issue-859"},
	}
	agentBackend := &fakeCodexClient{
		result: AgentTurnResult{ThreadID: "thread-fresh", TurnID: "turn-1", SessionID: "thread-fresh-turn-1"},
	}
	sessionStore := &fakeSessionStore{
		sessionID: 859,
		resumeState: store.AgentResumeState{
			DetentSessionID:   100,
			ProviderThreadID:  "thread-old",
			ProviderSessionID: "session-old",
		},
	}

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Work"},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Now:          newFakeClock(startedAt, startedAt.Add(time.Second)).Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-859",
			Identifier:    "digitaldrywood/detent#859",
			Title:         "Thread resume spike",
			ModelOverride: "gpt-5-codex",
		},
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if sessionStore.resumeLookups != 0 {
		t.Fatalf("resume lookups = %d, want 0 with flag disabled", sessionStore.resumeLookups)
	}
	if !agentResumeEmpty(agentBackend.request.Resume) {
		t.Fatalf("AgentTurnRequest.Resume = %#v, want empty with flag disabled", agentBackend.request.Resume)
	}
}

func TestRunnerRunRetryFreshSuppressesThreadResume(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 2, 16, 10, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-979", Branch: "detent/issue-979"},
	}
	agentBackend := &fakeCodexClient{
		result: AgentTurnResult{ThreadID: "thread-fresh", TurnID: "turn-1", SessionID: "thread-fresh-turn-1"},
	}
	sessionStore := &fakeSessionStore{
		sessionID: 979,
		resumeState: store.AgentResumeState{
			DetentSessionID:   100,
			ProviderThreadID:  "thread-old",
			ProviderSessionID: "session-old",
		},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{ExperimentalThreadResume: true},
			},
			Prompt: "Work",
		},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Now:          newFakeClock(startedAt, startedAt.Add(time.Second)).Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-979",
			Identifier:    "digitaldrywood/detent#979",
			Title:         "Recovery retry fresh",
			ModelOverride: "gpt-5-codex",
		},
		StartedAt: startedAt,
		RetryMode: RetryModeFresh,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if sessionStore.resumeLookups != 0 {
		t.Fatalf("resume lookups = %d, want 0 for retry fresh", sessionStore.resumeLookups)
	}
	if !agentResumeEmpty(agentBackend.request.Resume) {
		t.Fatalf("AgentTurnRequest.Resume = %#v, want empty for retry fresh", agentBackend.request.Resume)
	}
	if sessionStore.finished.ResumedFromSessionID != 0 {
		t.Fatalf("SessionFinish.ResumedFromSessionID = %d, want 0 for retry fresh", sessionStore.finished.ResumedFromSessionID)
	}
}

func TestRunnerRunRetryResumeUsesRequestedState(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 2, 16, 20, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-979", Branch: "detent/issue-979"},
	}
	agentBackend := &fakeCodexClient{
		result: AgentTurnResult{ThreadID: "thread-resumed", TurnID: "turn-1", SessionID: "thread-resumed-turn-1"},
	}
	sessionStore := &fakeSessionStore{
		sessionID: 980,
		resumeState: store.AgentResumeState{
			DetentSessionID:   101,
			ProviderThreadID:  "thread-unselected",
			ProviderSessionID: "session-unselected",
		},
	}
	selectedResume := store.AgentResumeState{
		DetentSessionID:   979,
		ProviderThreadID:  "thread-979",
		ProviderSessionID: "session-979",
	}
	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Work"},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Now:          newFakeClock(startedAt, startedAt.Add(time.Second)).Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-979",
			Identifier:    "digitaldrywood/detent#979",
			Title:         "Recovery retry resume",
			ModelOverride: "gpt-5-codex",
		},
		StartedAt:   startedAt,
		RetryMode:   RetryModeResume,
		ResumeState: selectedResume,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if sessionStore.resumeLookups != 0 {
		t.Fatalf("resume lookups = %d, want 0 for selected retry resume", sessionStore.resumeLookups)
	}
	if agentBackend.request.Resume.ThreadID != "thread-979" || agentBackend.request.Resume.SessionID != "session-979" {
		t.Fatalf("AgentTurnRequest.Resume = %#v, want selected resume state", agentBackend.request.Resume)
	}
	if sessionStore.finished.ResumedFromSessionID != 979 {
		t.Fatalf("SessionFinish.ResumedFromSessionID = %d, want selected session", sessionStore.finished.ResumedFromSessionID)
	}
}

func TestRunnerRunFallsBackFreshWhenResumeFails(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 2, 16, 30, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-859", Branch: "detent/issue-859"},
	}
	agentBackend := &resumeFallbackAgentBackend{}
	sessionStore := &fakeSessionStore{
		sessionID: 860,
		resumeState: store.AgentResumeState{
			DetentSessionID:   100,
			ProviderThreadID:  "thread-old",
			ProviderSessionID: "session-old",
		},
	}

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{ExperimentalThreadResume: true},
			},
			Prompt: "Work",
		},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Now:          newFakeClock(startedAt, startedAt.Add(time.Second)).Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-859",
			Identifier:    "digitaldrywood/detent#859",
			Title:         "Thread resume spike",
			ModelOverride: "gpt-5-codex",
		},
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want fresh fallback success", err)
	}
	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want completed", result.FinalState)
	}
	if sessionStore.resumeLookups != 1 {
		t.Fatalf("resume lookups = %d, want 1", sessionStore.resumeLookups)
	}
	if sessionStore.resumeLookup.AgentRole != RoleCode {
		t.Fatalf("resume lookup role = %q, want code", sessionStore.resumeLookup.AgentRole)
	}
	if len(agentBackend.requests) != 2 {
		t.Fatalf("backend requests = %d, want resumed attempt plus fresh fallback", len(agentBackend.requests))
	}
	if agentBackend.requests[0].Resume.ThreadID != "thread-old" || agentBackend.requests[0].Resume.SessionID != "session-old" {
		t.Fatalf("first request resume = %#v, want stored resume IDs", agentBackend.requests[0].Resume)
	}
	if !agentResumeEmpty(agentBackend.requests[1].Resume) {
		t.Fatalf("second request resume = %#v, want fresh fallback", agentBackend.requests[1].Resume)
	}
	if sessionStore.finished.ProviderThreadID != "thread-fresh" || sessionStore.finished.ProviderSessionID != "session-fresh" {
		t.Fatalf("SessionFinish provider IDs = %#v, want fresh IDs", sessionStore.finished)
	}
	if sessionStore.finished.ResumedFromSessionID != 0 {
		t.Fatalf("SessionFinish.ResumedFromSessionID = %d, want 0 after fallback", sessionStore.finished.ResumedFromSessionID)
	}
}

func TestRunnerRunDoesNotFallbackAfterResumedTurnStarts(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 2, 16, 45, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-859", Branch: "detent/issue-859"},
	}
	agentBackend := &resumeStartedFailureAgentBackend{}
	sessionStore := &fakeSessionStore{
		sessionID: 861,
		resumeState: store.AgentResumeState{
			DetentSessionID:   100,
			ProviderThreadID:  "thread-old",
			ProviderSessionID: "session-old",
		},
	}

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{ExperimentalThreadResume: true},
			},
			Prompt: "Work",
		},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Now:          newFakeClock(startedAt, startedAt.Add(time.Second)).Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-859",
			Identifier:    "digitaldrywood/detent#859",
			Title:         "Thread resume spike",
			ModelOverride: "gpt-5-codex",
		},
		StartedAt: startedAt,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want resumed turn failure")
	}
	if len(agentBackend.requests) != 1 {
		t.Fatalf("backend requests = %d, want no fresh fallback after turn start", len(agentBackend.requests))
	}
	if agentBackend.requests[0].Resume.ThreadID != "thread-old" || agentBackend.requests[0].Resume.SessionID != "session-old" {
		t.Fatalf("request resume = %#v, want stored resume IDs", agentBackend.requests[0].Resume)
	}
	if sessionStore.finished.FinalState != FinalStateFailed {
		t.Fatalf("SessionFinish.FinalState = %q, want failed", sessionStore.finished.FinalState)
	}
	if sessionStore.finished.ResumedFromSessionID != 100 {
		t.Fatalf("SessionFinish.ResumedFromSessionID = %d, want resumed source", sessionStore.finished.ResumedFromSessionID)
	}
}

func TestRunnerRunKillsSessionAtTokenCeilingAndRecordsLesson(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	startedAt := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: workspacePath, Key: "issue-853", Branch: "detent/issue-853"},
	}
	agentBackend := &fakeCodexClient{
		updates: []AgentUpdate{
			{
				Type:     AgentUpdateTokenUsage,
				ThreadID: "thread-853",
				TurnID:   "turn-1",
				Tokens: AgentTokenUsage{
					InputTokens:  80,
					OutputTokens: 10,
					TotalTokens:  90,
				},
			},
			{
				Type:     AgentUpdateTokenUsage,
				ThreadID: "thread-853",
				TurnID:   "turn-1",
				Tokens: AgentTokenUsage{
					InputTokens:  100,
					OutputTokens: 20,
					TotalTokens:  120,
				},
			},
		},
	}
	sessionStore := &fakeSessionStore{sessionID: 853}
	clock := newFakeClock(
		startedAt,
		startedAt.Add(time.Second),
		startedAt.Add(2*time.Second),
		startedAt.Add(3*time.Second),
	)

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{
					MaxSessionTokens: 100,
					Lessons: config.Lessons{
						Path:       ".detent/lessons.md",
						MaxEntries: 5,
					},
				},
			},
			Prompt: "Work on {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Store:        sessionStore,
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	var usageUpdates []UsageUpdate
	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-853",
			Identifier: "digitaldrywood/detent#853",
			Title:      "Per-session token ceiling",
		},
		StartedAt: startedAt,
		OnUsageUpdate: func(update UsageUpdate) error {
			usageUpdates = append(usageUpdates, update)
			return nil
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want token ceiling error")
	}
	if !errors.Is(err, ErrSessionTokenCeilingExceeded) {
		t.Fatalf("Run() error = %v, want ErrSessionTokenCeilingExceeded", err)
	}
	var ceilingErr *SessionTokenCeilingError
	if !errors.As(err, &ceilingErr) {
		t.Fatalf("Run() error = %T, want SessionTokenCeilingError", err)
	}
	if ceilingErr.TotalTokens != 120 || ceilingErr.CeilingTokens != 100 || ceilingErr.Source != TokenCeilingSourceAbsolute {
		t.Fatalf("ceiling error = %#v, want total 120 ceiling 100 absolute source", ceilingErr)
	}
	if result.FinalState != FinalStateTokenCeilingExceeded {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateTokenCeilingExceeded)
	}
	if sessionStore.finished.FinalState != FinalStateTokenCeilingExceeded || sessionStore.finished.TotalTokens != 120 {
		t.Fatalf("SessionFinish = %#v, want token ceiling final state and 120 tokens", sessionStore.finished)
	}
	if sessionStore.usage.Outcome != FinalStateTokenCeilingExceeded || sessionStore.usage.TotalTokens != 120 {
		t.Fatalf("UsageEvent = %#v, want token ceiling outcome and 120 tokens", sessionStore.usage)
	}
	if sessionStore.phase.Status != FinalStateTokenCeilingExceeded || sessionStore.phase.TotalTokens != 120 {
		t.Fatalf("WorkflowPhaseEvent = %#v, want token ceiling status and 120 tokens", sessionStore.phase)
	}
	if len(usageUpdates) != 3 {
		t.Fatalf("usage update count = %d, want configured identity plus 2 token updates", len(usageUpdates))
	}
	if got := usageUpdates[len(usageUpdates)-1].Tokens.TotalTokens; got != 120 {
		t.Fatalf("last live usage total tokens = %d, want ceiling-crossing 120", got)
	}

	lesson, err := os.ReadFile(filepath.Join(workspacePath, ".detent", "lessons.md"))
	if err != nil {
		t.Fatalf("ReadFile(lessons) error = %v", err)
	}
	for _, want := range []string{
		"Failure kind:** token_ceiling_exceeded",
		"session reached 120 tokens",
		"configured ceiling 100",
	} {
		if !strings.Contains(string(lesson), want) {
			t.Fatalf("lesson missing %q:\n%s", want, lesson)
		}
	}
}

func TestRunnerRunLeavesSessionTokenCeilingDisabledByDefault(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 2, 14, 30, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-default", Branch: "detent/issue-default"},
	}
	contextWindow := int64(100)
	agentBackend := &fakeCodexClient{
		updates: []AgentUpdate{{
			Type:     AgentUpdateTokenUsage,
			ThreadID: "thread-default",
			TurnID:   "turn-1",
			Tokens: AgentTokenUsage{
				InputTokens:        1000000,
				OutputTokens:       250000,
				TotalTokens:        1250000,
				ModelContextWindow: &contextWindow,
			},
		}},
	}
	clock := newFakeClock(startedAt, startedAt.Add(time.Second), startedAt.Add(2*time.Second))

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Work"},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-default",
			Identifier: "digitaldrywood/detent#854",
			Title:      "Default behavior",
		},
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FinalState != FinalStateCompleted || result.Tokens.TotalTokens != 1250000 {
		t.Fatalf("Run() result = %#v, want completed with large token total", result)
	}
}

func TestRunnerRunSessionTokenOverrideBypassesLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cfg   config.Agent
		issue connector.Issue
	}{
		{
			name: "label",
			cfg: config.Agent{
				MaxSessionTokens:             100,
				MaxSessionTokenOverrideLabel: "allow-large-session",
			},
			issue: connector.Issue{
				ID:         "issue-label",
				Identifier: "digitaldrywood/detent#855",
				Title:      "Large label session",
				Labels:     []string{"Allow-Large-Session"},
			},
		},
		{
			name: "field",
			cfg: config.Agent{
				MaxSessionTokens:             100,
				MaxSessionTokenOverrideField: "Token Override",
			},
			issue: connector.Issue{
				ID:         "issue-field",
				Identifier: "digitaldrywood/detent#856",
				Title:      "Large field session",
				Fields:     map[string]string{"Token Override": "true"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			startedAt := time.Date(2026, 7, 2, 15, 0, 0, 0, time.UTC)
			agentBackend := &fakeCodexClient{
				updates: []AgentUpdate{{
					Type:     AgentUpdateTokenUsage,
					ThreadID: "thread-override",
					TurnID:   "turn-1",
					Tokens: AgentTokenUsage{
						InputTokens:  100,
						OutputTokens: 20,
						TotalTokens:  120,
					},
				}},
			}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{
					Config: config.Config{Agent: tt.cfg},
					Prompt: "Work",
				},
				Workspace: &fakeWorkspaceBackend{
					info: workspace.Info{Path: t.TempDir(), Key: tt.issue.ID, Branch: "detent/" + tt.issue.ID},
				},
				AgentBackend: agentBackend,
				Now:          newFakeClock(startedAt, startedAt.Add(time.Second), startedAt.Add(2*time.Second)).Now,
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			result, err := runner.Run(context.Background(), RunRequest{Issue: tt.issue, StartedAt: startedAt})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.FinalState != FinalStateCompleted || result.Tokens.TotalTokens != 120 {
				t.Fatalf("Run() result = %#v, want completed with 120 tokens", result)
			}
		})
	}
}

func TestSessionTokenCeilingForUsageUsesTightestConfiguredLimit(t *testing.T) {
	t.Parallel()

	contextWindow := int64(1000)
	tests := []struct {
		name   string
		cfg    config.Agent
		tokens AgentTokenUsage
		want   sessionTokenCeiling
		ok     bool
	}{
		{
			name:   "disabled",
			cfg:    config.Agent{},
			tokens: AgentTokenUsage{TotalTokens: 1000000, ModelContextWindow: &contextWindow},
		},
		{
			name:   "absolute",
			cfg:    config.Agent{MaxSessionTokens: 5000},
			tokens: AgentTokenUsage{ModelContextWindow: &contextWindow},
			want:   sessionTokenCeiling{tokens: 5000, source: TokenCeilingSourceAbsolute},
			ok:     true,
		},
		{
			name:   "context multiplier",
			cfg:    config.Agent{MaxSessionContextMultiplier: 2.5},
			tokens: AgentTokenUsage{ModelContextWindow: &contextWindow},
			want: sessionTokenCeiling{
				tokens:             2500,
				source:             TokenCeilingSourceContextWindow,
				modelContextWindow: 1000,
				contextMultiplier:  2.5,
			},
			ok: true,
		},
		{
			name:   "tighter context multiplier",
			cfg:    config.Agent{MaxSessionTokens: 5000, MaxSessionContextMultiplier: 2},
			tokens: AgentTokenUsage{ModelContextWindow: &contextWindow},
			want: sessionTokenCeiling{
				tokens:             2000,
				source:             TokenCeilingSourceContextWindow,
				modelContextWindow: 1000,
				contextMultiplier:  2,
			},
			ok: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := sessionTokenCeilingForUsage(tt.cfg, tt.tokens)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("sessionTokenCeilingForUsage() = %#v, %v; want %#v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRunnerMergeModeCleanPrecheckSkipsAgent(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	workspaceBackend := &fakeMergeWorkspaceBackend{
		fakeWorkspaceBackend: fakeWorkspaceBackend{
			info: workspace.Info{
				Path:   workspacePath,
				Key:    "digitaldrywood_detent_860",
				Branch: "detent/digitaldrywood_detent_860",
			},
		},
		prepareResult: workspace.MergePrepareResult{Status: workspace.MergePrepareStatusClean},
	}
	codexClient := &fakeCodexClient{}
	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-860",
			Identifier: "digitaldrywood/detent#860",
			BranchName: "detent/digitaldrywood_detent_860",
		},
		Mode: RunModeMerge,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want completed", result.FinalState)
	}
	if !workspaceBackend.prepareCalled {
		t.Fatal("PrepareMerge() was not called")
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun() was not called")
	}
	if codexClient.request.Prompt != "" {
		t.Fatalf("agent prompt = %q, want no agent dispatch", codexClient.request.Prompt)
	}
}

func TestRunnerMergeModeConflictUsesFocusedPrompt(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	workspaceBackend := &fakeMergeWorkspaceBackend{
		fakeWorkspaceBackend: fakeWorkspaceBackend{
			info: workspace.Info{
				Path:   workspacePath,
				Key:    "digitaldrywood_detent_860",
				Branch: "detent/digitaldrywood_detent_860",
			},
		},
		prepareResult: workspace.MergePrepareResult{
			Status:  workspace.MergePrepareStatusConflict,
			Message: "CONFLICT (content): Merge conflict in README.md",
		},
	}
	codexClient := &fakeCodexClient{
		result: AgentTurnResult{ThreadID: "thread-merge", TurnID: "turn-1"},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{},
			Prompt: "Full implement workflow playbook for {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-860",
			Identifier: "digitaldrywood/detent#860",
			Title:      "Deterministic merge fast-path",
			BranchName: "detent/digitaldrywood_detent_860",
			PullRequest: &connector.PullRequest{
				URL: "https://github.com/digitaldrywood/detent/pull/900",
			},
		},
		Mode: RunModeMerge,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	prompt := codexClient.request.Prompt
	for _, want := range []string{
		"merge-worker fallback",
		"Deterministic merge pre-check status: conflict",
		"CONFLICT (content): Merge conflict in README.md",
		"Re-run the fetch/rebase onto the target branch",
		"https://github.com/digitaldrywood/detent/pull/900",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Full implement workflow playbook") {
		t.Fatalf("prompt included full workflow playbook:\n%s", prompt)
	}
}

func TestRunnerRunAddsGitMetadataExtraRootsForManagedWorkspace(t *testing.T) {
	t.Parallel()

	source := initRunnerSourceRepo(t)
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workspaceBackend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		Root:       workspaceRoot,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	agentBackend := &committingAgentBackend{}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{},
			Prompt: "Work on {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-743",
			Identifier: "digitaldrywood/detent#743",
			Title:      "Managed workspace sandbox can prevent git add/commit",
			BranchName: "detent/issue-743",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateCompleted)
	}

	wantRoots, err := workspace.GitMetadataWritableRoots(context.Background(), agentBackend.request.Workspace)
	if err != nil {
		t.Fatalf("GitMetadataWritableRoots() error = %v", err)
	}
	gotRoots := agentBackend.request.ExtraWritableRoots
	for _, want := range wantRoots {
		if !containsRunnerString(gotRoots, want) {
			t.Fatalf("extra roots = %#v, missing %q", gotRoots, want)
		}
	}
	if got := strings.TrimSpace(runRunnerGit(t, agentBackend.request.Workspace, "log", "-1", "--pretty=%s")); got != "agent commit" {
		t.Fatalf("latest commit subject = %q, want agent commit", got)
	}
}

func TestRunnerRunLogsLifecycleWithoutPromptOrMessageBody(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	var logs bytes.Buffer
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{
			{Type: AgentUpdateProcessStarted, ProcessIdentity: "pid-123"},
			{
				Type:            AgentUpdateTurnStarted,
				ThreadID:        "thread-1",
				TurnID:          "turn-1",
				Model:           "gpt-5.6-sol",
				RuntimeIdentity: agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{}),
			},
			{Type: AgentUpdateMessageDelta, Delta: "do not log this message body"},
			{Type: AgentUpdateTokenUsage, ThreadID: "thread-1", TurnID: "turn-1", Tokens: AgentTokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
			{Type: AgentUpdateTurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Status: "completed"},
		},
		result: AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1", SessionID: "session-1"},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{Config: config.Config{}},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: workspacePath, Key: "issue-726", Branch: "detent/issue-726"},
		},
		AgentBackend: codexClient,
		Store:        &fakeSessionStore{sessionID: 726},
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:          "issue-726",
			Identifier:  "digitaldrywood/detent#726",
			Title:       "Lifecycle diagnostics",
			Description: "do not log this prompt body",
			State:       "Todo",
		},
		WorkAttemptID: 88,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	logText := logs.String()
	for _, fragment := range []string{
		"worker_workspace_create_started",
		"worker_workspace_created",
		"worker_before_run_finished",
		"worker_session_started",
		"worker_command_started",
		"worker_runtime_identity_resolved",
		"worker_process_started",
		"worker_turn_started",
		"worker_usage_updated",
		"worker_turn_finished",
		"worker_command_finished",
		"worker_after_run_finished",
		"worker_session_finished",
		"issue_id=issue-726",
		"work_attempt_id=88",
		"provider_thread_id=thread-1",
		"provider_session_id=thread-1-turn-1",
		"backend_kind=codex",
		"provider=openai",
		"provider_provenance=runtime",
		"resolved_model=gpt-5.6-sol",
		"resolved_model_provenance=runtime",
		"reasoning_effort=xhigh",
		"service_tier=priority",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs missing %q:\n%s", fragment, logText)
		}
	}
	for _, leaked := range []string{"do not log this message body", "do not log this prompt body"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("logs leaked %q:\n%s", leaked, logText)
		}
	}
}

func TestLogRuntimeIdentityChangeUsesCanonicalFieldsWithoutPayloadSecrets(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	r := &Runner{
		projectID: "detent",
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
	previous := agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
		Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "high", "priority", time.Time{}))
	current := previous.Merge(agentidentity.RuntimeUpdate("gpt-5.6-terra", "openai", "xhigh", "priority", time.Time{}))
	r.logRuntimeIdentity(
		RunRequest{
			Issue:         connector.Issue{ID: "issue-1118", Identifier: "digitaldrywood/detent#1118"},
			WorkAttemptID: 73,
		},
		1118,
		AgentUpdate{
			Method:           "model/rerouted",
			ThreadID:         "thread-1118",
			TurnID:           "turn-2",
			BackendErrorBody: `{"base_url":"https://secret.example","authorization":"Bearer secret"}`,
		},
		previous,
		current,
	)

	got := logs.String()
	for _, want := range []string{
		"worker_runtime_identity_changed",
		"project_id=detent",
		"issue_id=issue-1118",
		"work_attempt_id=73",
		"detent_session_id=1118",
		"provider_thread_id=thread-1118",
		"provider_session_id=thread-1118-turn-2",
		"old_resolved_model=gpt-5.6-sol",
		"new_resolved_model=gpt-5.6-terra",
		"old_reasoning_effort=high",
		"new_reasoning_effort=xhigh",
		"new_reasoning_effort_provenance=runtime",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("runtime identity log missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"secret.example", "Bearer secret", "authorization", "base_url"} {
		if strings.Contains(got, secret) {
			t.Fatalf("runtime identity log leaked %q:\n%s", secret, got)
		}
	}
}

func TestRunnerRunCompletesSuccessfulTurnWithCleanupError(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	var logs bytes.Buffer
	sessionStore := &fakeSessionStore{sessionID: 970}
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{
			{Type: AgentUpdateTurnStarted, ThreadID: "thread-970", TurnID: "turn-1"},
			{Type: AgentUpdateTurnCompleted, ThreadID: "thread-970", TurnID: "turn-1", Status: "completed"},
		},
		result: AgentTurnResult{ThreadID: "thread-970", TurnID: "turn-1", SessionID: "thread-970-turn-1"},
		err:    NewAgentTurnCleanupError(errors.New("close codex app-server transport: operation not permitted")),
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{Config: config.Config{}},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: workspacePath, Key: "issue-970", Branch: "detent/issue-970"},
		},
		AgentBackend: codexClient,
		Store:        sessionStore,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-970",
			Identifier: "digitaldrywood/detent#970",
			Title:      "Handle stale successful CI checks",
			State:      "Todo",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want completed", result.FinalState)
	}
	if sessionStore.finished.FinalState != FinalStateCompleted {
		t.Fatalf("SessionFinish.FinalState = %q, want completed", sessionStore.finished.FinalState)
	}

	logText := logs.String()
	if !strings.Contains(logText, "cleanup_error") || !strings.Contains(logText, "operation not permitted") {
		t.Fatalf("logs missing cleanup warning:\n%s", logText)
	}
	if strings.Contains(logText, "outcome=failed") {
		t.Fatalf("logs reported failed outcome for cleanup-only error:\n%s", logText)
	}
}

func TestRunnerPlanModeCapturesOutputAndConstrainsPrompt(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-521", Branch: "detent/issue-521"},
	}
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{
			{Type: AgentUpdateMessageDelta, ThreadID: "thread-plan", TurnID: "turn-plan", ItemID: "message-plan", Delta: "## Plan\n"},
			{Type: AgentUpdateMessageDelta, ThreadID: "thread-plan", TurnID: "turn-plan", ItemID: "message-plan", Delta: "- Add tests\n"},
		},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{},
			Prompt: "Implement {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-521",
			Identifier: "digitaldrywood/detent#521",
			Title:      "Plan stop",
		},
		Mode: RunModePlan,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "## Plan\n- Add tests\n" {
		t.Fatalf("Output = %q, want completed assistant plan", result.Output)
	}
	for _, want := range []string{
		"## Plan approval stop",
		"Do not modify files",
		"Do not move tracker state",
		"structured implementation plan",
	} {
		if !strings.Contains(codexClient.request.Prompt, want) {
			t.Fatalf("plan prompt missing %q:\n%s", want, codexClient.request.Prompt)
		}
	}
}

func TestRunRoleDerivesStageFromModeAndState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		mode  string
		state string
		want  string
	}{
		{name: "empty mode todo uses code", state: "Todo", want: RoleCode},
		{name: "implement mode in progress uses code", mode: RunModeImplement, state: "In Progress", want: RoleCode},
		{name: "plan mode uses plan", mode: RunModePlan, state: "Todo", want: RolePlan},
		{name: "plan mode overrides rework state", mode: RunModePlan, state: "Rework", want: RolePlan},
		{name: "rework state uses rework", mode: RunModeImplement, state: "Rework", want: RoleRework},
		{name: "rework state trims and folds case", mode: RunModeImplement, state: " reWORK ", want: RoleRework},
		{name: "merging state uses merge", mode: RunModeImplement, state: "Merging", want: RoleMerge},
		{name: "unknown mode normalizes to implement", mode: "unknown", state: "Merging", want: RoleMerge},
		{name: "observed review state uses code", mode: RunModeImplement, state: "Human Review", want: RoleCode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runRole(tt.mode, connector.Issue{State: tt.state})
			if got != tt.want {
				t.Fatalf("runRole(%q, %q) = %q, want %q", tt.mode, tt.state, got, tt.want)
			}
		})
	}
}

func TestRunnerRunRoutesPerStageRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mode        string
		state       string
		wantBackend string
		wantModel   string
		wantRole    string
	}{
		{name: "code default", state: "Todo", wantBackend: "codex-code", wantModel: "gpt-5-code", wantRole: RoleCode},
		{name: "plan mode", mode: RunModePlan, state: "Todo", wantBackend: "codex-plan", wantModel: "gpt-5-plan", wantRole: RolePlan},
		{name: "rework state", mode: RunModeImplement, state: "Rework", wantBackend: "codex-rework", wantModel: "gpt-5-rework", wantRole: RoleRework},
		{name: "merge mode", mode: RunModeMerge, state: "Merging", wantBackend: "codex-merge", wantModel: "gpt-5-merge", wantRole: RoleMerge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspaceBackend := &fakeWorkspaceBackend{
				info: workspace.Info{Path: t.TempDir(), Key: "issue-stage", Branch: "detent/issue-stage"},
			}
			clients := map[string]*fakeCodexClient{
				"codex-code":   {},
				"codex-plan":   {},
				"codex-rework": {},
				"codex-merge":  {},
			}
			sessionStore := &fakeSessionStore{sessionID: 861}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{
					Config: config.Config{
						Agents: config.Agents{
							Backends: []config.AgentBackend{
								{ID: "codex-code", Kind: "codex", Protocol: "app-server", Command: "codex app-server"},
								{ID: "codex-plan", Kind: "codex", Protocol: "app-server", Command: "codex app-server --profile plan"},
								{ID: "codex-rework", Kind: "codex", Protocol: "app-server", Command: "codex app-server --profile rework"},
								{ID: "codex-merge", Kind: "codex", Protocol: "app-server", Command: "codex app-server --profile merge"},
							},
							Routes: []config.AgentRoute{
								{Name: "plan", Role: RolePlan, Backend: "codex-plan", Model: "gpt-5-plan"},
								{Name: "rework", Role: RoleRework, Backend: "codex-rework", Model: "gpt-5-rework"},
								{Name: "merge", Role: RoleMerge, Backend: "codex-merge", Model: "gpt-5-merge"},
								{Name: "default", Backend: "codex-code", Model: "gpt-5-code", Default: true},
							},
						},
					},
					Prompt: "work {{ issue.identifier }}",
				},
				Workspace: workspaceBackend,
				AgentBackends: map[string]AgentBackend{
					"codex-code":   clients["codex-code"],
					"codex-plan":   clients["codex-plan"],
					"codex-rework": clients["codex-rework"],
					"codex-merge":  clients["codex-merge"],
				},
				Store: sessionStore,
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			_, err = runner.Run(context.Background(), RunRequest{
				Issue: connector.Issue{
					ID:         "issue-stage",
					Identifier: "digitaldrywood/detent#861",
					Title:      "Per-stage roles",
					State:      tt.state,
				},
				Mode: tt.mode,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			for backendID, client := range clients {
				wantCalls := 0
				if backendID == tt.wantBackend {
					wantCalls = 1
				}
				if client.calls != wantCalls {
					t.Fatalf("%s calls = %d, want %d", backendID, client.calls, wantCalls)
				}
			}
			if clients[tt.wantBackend].request.Model != tt.wantModel {
				t.Fatalf("Model = %q, want %q", clients[tt.wantBackend].request.Model, tt.wantModel)
			}
			if sessionStore.started.AgentRole != tt.wantRole {
				t.Fatalf("SessionStart.AgentRole = %q, want %q", sessionStore.started.AgentRole, tt.wantRole)
			}
			if sessionStore.started.Model != "" || sessionStore.started.RequestedModel != tt.wantModel {
				t.Fatalf("SessionStart = %#v, want unresolved model and requested %q", sessionStore.started, tt.wantModel)
			}
			if sessionStore.usage.SessionID != 861 {
				t.Fatalf("UsageEvent.SessionID = %d, want role-bearing session 861", sessionStore.usage.SessionID)
			}
			if sessionStore.phase.SessionID != 861 {
				t.Fatalf("WorkflowPhaseEvent.SessionID = %d, want role-bearing session 861", sessionStore.phase.SessionID)
			}
		})
	}
}

func TestRunnerRunThreadResumeUsesDerivedRoleKey(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-903", Branch: "detent/issue-903"},
	}
	agentBackend := &fakeCodexClient{
		result: AgentTurnResult{ThreadID: "thread-merge", TurnID: "turn-merge", SessionID: "session-merge"},
	}
	sessionStore := &fakeSessionStore{
		sessionID: 903,
		resumeStates: map[string]store.AgentResumeState{
			RoleCode: {
				DetentSessionID:   100,
				ProviderThreadID:  "thread-code",
				ProviderSessionID: "session-code",
			},
		},
	}

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{ExperimentalThreadResume: true},
				Agents: config.Agents{
					Backends: []config.AgentBackend{{
						ID:       "codex-code",
						Kind:     "codex",
						Protocol: "app-server",
						Command:  "codex app-server",
					}},
					Routes: []config.AgentRoute{{
						Name:    "default",
						Backend: "codex-code",
						Model:   "gpt-5-code",
						Default: true,
					}},
				},
			},
			Prompt: "work {{ issue.identifier }}",
		},
		Workspace:     workspaceBackend,
		AgentBackends: map[string]AgentBackend{"codex-code": agentBackend},
		Store:         sessionStore,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-903",
			Identifier: "digitaldrywood/detent#903",
			State:      "Merging",
		},
		Mode: RunModeMerge,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if sessionStore.resumeLookup.AgentRole != RoleMerge {
		t.Fatalf("resume lookup role = %q, want %q", sessionStore.resumeLookup.AgentRole, RoleMerge)
	}
	if !agentResumeEmpty(agentBackend.request.Resume) {
		t.Fatalf("AgentTurnRequest.Resume = %#v, want no implement resume state for merge", agentBackend.request.Resume)
	}
	if sessionStore.started.AgentRole != RoleMerge {
		t.Fatalf("SessionStart.AgentRole = %q, want %q", sessionStore.started.AgentRole, RoleMerge)
	}
	if sessionStore.started.Model != "" || sessionStore.started.RequestedModel != "gpt-5-code" {
		t.Fatalf("SessionStart = %#v, want unresolved model and fallback code request", sessionStore.started)
	}
}

func TestRunnerRunUsesStageDefaultModelForReworkFallback(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-903", Branch: "detent/issue-903"},
	}
	agentBackend := &fakeCodexClient{}
	sessionStore := &fakeSessionStore{sessionID: 904}
	checker := &fakeBudgetChecker{}
	estimator := &fakeDispatchEstimator{}

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agents: config.Agents{
					Backends: []config.AgentBackend{
						{ID: "codex-code", Kind: "codex", Protocol: "app-server", Command: "codex app-server"},
						{ID: "codex-rework", Kind: "codex", Protocol: "app-server", Command: "codex app-server --profile rework"},
					},
					Routes: []config.AgentRoute{
						{
							Name:    "rework-selector",
							Role:    RoleRework,
							Backend: "codex-rework",
							Selector: selector.Selector{
								Labels: selector.Labels{Include: []string{"needs-rework"}},
							},
						},
						{Name: "rework-default", Role: RoleRework, Backend: "codex-rework", Model: "gpt-5-rework-default", Default: true},
						{Name: "default", Backend: "codex-code", Model: "gpt-5-code-default", Default: true},
					},
				},
			},
			Prompt: "work {{ issue.identifier }}",
		},
		Workspace:         workspaceBackend,
		AgentBackends:     map[string]AgentBackend{"codex-code": &fakeCodexClient{}, "codex-rework": agentBackend},
		Store:             sessionStore,
		BudgetChecker:     checker,
		DispatchEstimator: estimator,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-903",
			Identifier: "digitaldrywood/detent#903",
			State:      "Rework",
			Labels:     []string{"needs-rework"},
		},
		Mode: RunModeImplement,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if estimator.model != "gpt-5-rework-default" {
		t.Fatalf("dispatch estimate model = %q, want rework default", estimator.model)
	}
	if checker.model != "gpt-5-rework-default" {
		t.Fatalf("budget check model = %q, want rework default", checker.model)
	}
	if sessionStore.started.Model != "" || sessionStore.started.RequestedModel != "gpt-5-rework-default" {
		t.Fatalf("SessionStart = %#v, want unresolved model and rework default request", sessionStore.started)
	}
	if sessionStore.started.AgentRole != RoleRework {
		t.Fatalf("SessionStart.AgentRole = %q, want %q", sessionStore.started.AgentRole, RoleRework)
	}
}

func TestRunnerRunUnroutedStageRolesUseCodeDefaultRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     string
		state    string
		wantRole string
	}{
		{name: "plan role", mode: RunModePlan, state: "Todo", wantRole: RolePlan},
		{name: "rework role", mode: RunModeImplement, state: "Rework", wantRole: RoleRework},
		{name: "merge role", mode: RunModeImplement, state: "Merging", wantRole: RoleMerge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspaceBackend := &fakeWorkspaceBackend{
				info: workspace.Info{Path: t.TempDir(), Key: "issue-fallback", Branch: "detent/issue-fallback"},
			}
			backend := &fakeCodexClient{}
			sessionStore := &fakeSessionStore{sessionID: 862}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{
					Config: config.Config{
						Agents: config.Agents{
							Backends: []config.AgentBackend{{
								ID:       "codex-code",
								Kind:     "codex",
								Protocol: "app-server",
								Command:  "codex app-server",
							}},
							Routes: []config.AgentRoute{{
								Name:    "default",
								Backend: "codex-code",
								Model:   "gpt-5-code",
								Default: true,
							}},
						},
					},
					Prompt: "work {{ issue.identifier }}",
				},
				Workspace: workspaceBackend,
				AgentBackends: map[string]AgentBackend{
					"codex-code": backend,
				},
				Store: sessionStore,
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			_, err = runner.Run(context.Background(), RunRequest{
				Issue: connector.Issue{
					ID:         "issue-fallback",
					Identifier: "digitaldrywood/detent#861",
					Title:      "Per-stage fallback",
					State:      tt.state,
				},
				Mode: tt.mode,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if backend.calls != 1 {
				t.Fatalf("RunTurn calls = %d, want 1", backend.calls)
			}
			if backend.request.Model != "gpt-5-code" {
				t.Fatalf("Model = %q, want code default model", backend.request.Model)
			}
			if sessionStore.started.AgentRole != tt.wantRole {
				t.Fatalf("SessionStart.AgentRole = %q, want %q", sessionStore.started.AgentRole, tt.wantRole)
			}
			if sessionStore.started.Model != "" || sessionStore.started.RequestedModel != "gpt-5-code" {
				t.Fatalf("SessionStart = %#v, want unresolved model and fallback code request", sessionStore.started)
			}
		})
	}
}

func TestRunnerRunUnroutedStageRolesUseCodeSelectorRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		mode  string
		state string
	}{
		{name: "plan role", mode: RunModePlan, state: "Todo"},
		{name: "rework role", mode: RunModeImplement, state: "Rework"},
		{name: "merge role", mode: RunModeImplement, state: "Merging"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspaceBackend := &fakeWorkspaceBackend{
				info: workspace.Info{Path: t.TempDir(), Key: "issue-selector", Branch: "detent/issue-selector"},
			}
			codeBackend := &fakeCodexClient{}
			highBackend := &fakeCodexClient{}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{
					Config: config.Config{
						Agents: config.Agents{
							Backends: []config.AgentBackend{
								{ID: "codex-code", Kind: "codex", Protocol: "app-server", Command: "codex app-server"},
								{ID: "codex-high", Kind: "codex", Protocol: "app-server", Command: "codex app-server --profile high"},
							},
							Routes: []config.AgentRoute{
								{
									Name:    "high-label",
									Backend: "codex-high",
									Model:   "gpt-5-high",
									Selector: selector.Selector{
										Labels: selector.Labels{Include: []string{"tier:high"}},
									},
								},
								{Name: "default", Backend: "codex-code", Model: "gpt-5-code", Default: true},
							},
						},
					},
					Prompt: "work {{ issue.identifier }}",
				},
				Workspace: workspaceBackend,
				AgentBackends: map[string]AgentBackend{
					"codex-code": codeBackend,
					"codex-high": highBackend,
				},
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			_, err = runner.Run(context.Background(), RunRequest{
				Issue: connector.Issue{
					ID:         "issue-selector",
					Identifier: "digitaldrywood/detent#861",
					Title:      "Per-stage selector fallback",
					State:      tt.state,
					Labels:     []string{"tier:high"},
				},
				Mode: tt.mode,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if highBackend.calls != 1 {
				t.Fatalf("high backend calls = %d, want 1", highBackend.calls)
			}
			if codeBackend.calls != 0 {
				t.Fatalf("code backend calls = %d, want 0", codeBackend.calls)
			}
			if highBackend.request.Model != "gpt-5-high" {
				t.Fatalf("Model = %q, want code selector model", highBackend.request.Model)
			}
		})
	}
}

func TestRunnerRunUnroutedStageRolesUseCodeModelFieldRouteWithoutDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		mode  string
		state string
	}{
		{name: "plan role", mode: RunModePlan, state: "Todo"},
		{name: "rework role", mode: RunModeImplement, state: "Rework"},
		{name: "merge role", mode: RunModeImplement, state: "Merging"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspaceBackend := &fakeWorkspaceBackend{
				info: workspace.Info{Path: t.TempDir(), Key: "issue-field", Branch: "detent/issue-field"},
			}
			backend := &fakeCodexClient{}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{
					Config: config.Config{
						Agents: config.Agents{
							Backends: []config.AgentBackend{{
								ID:       "codex-code",
								Kind:     "codex",
								Protocol: "app-server",
								Command:  "codex app-server",
							}},
							Routes: []config.AgentRoute{{
								Name:       "board-model",
								Backend:    "codex-code",
								ModelField: "Model",
							}},
						},
					},
					Prompt: "work {{ issue.identifier }}",
				},
				Workspace: workspaceBackend,
				AgentBackends: map[string]AgentBackend{
					"codex-code": backend,
				},
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			_, err = runner.Run(context.Background(), RunRequest{
				Issue: connector.Issue{
					ID:         "issue-field",
					Identifier: "digitaldrywood/detent#861",
					Title:      "Per-stage model field fallback",
					State:      tt.state,
					Fields:     map[string]string{"Model": "gpt-5-field"},
				},
				Mode: tt.mode,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if backend.calls != 1 {
				t.Fatalf("RunTurn calls = %d, want 1", backend.calls)
			}
			if backend.request.Model != "gpt-5-field" {
				t.Fatalf("Model = %q, want code model_field model", backend.request.Model)
			}
		})
	}
}

func TestRunnerUsageCostUsesFallbackForUnknownModel(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runner := &Runner{
		pricing: budget.PricingTable{},
		logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	}

	cost := runner.usageCostUSD(" missing-model ", 10, 2, 5, "codex")
	if cost == 0 {
		t.Fatal("usageCostUSD() = 0, want fallback pricing")
	}
	if got := logs.String(); strings.Contains(got, "usage event model pricing not found") {
		t.Fatalf("log output = %q, want no unknown pricing warning", got)
	}
}

func TestRunnerUsageCostSkipsPricingWarningForEmptyModel(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runner := &Runner{
		pricing: budget.PricingTable{},
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}

	cost := runner.usageCostUSD(" \t", 10, 2, 5, "claude_code")
	if cost == 0 {
		t.Fatal("usageCostUSD() = 0, want fallback pricing")
	}
	got := logs.String()
	if strings.Contains(got, "level=WARN") || strings.Contains(got, "usage event model pricing not found") {
		t.Fatalf("log output = %q, want no empty-model pricing warning", got)
	}
	if !strings.Contains(got, "usage event model unavailable; using fallback pricing") {
		t.Fatalf("log output = %q, want empty-model diagnostic", got)
	}
	if !strings.Contains(got, "backend_kind=claude_code") {
		t.Fatalf("log output = %q, want backend kind diagnostic", got)
	}
}

func TestRunnerValidateUsesValidatorRouteModelOverrideAndParsesJSON(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{
			Path:   workspacePath,
			Key:    "digitaldrywood_detent_522",
			Branch: "detent/digitaldrywood_detent_522",
		},
	}
	validatorBackend := &fakeCodexClient{
		updates: []AgentUpdate{{
			Type:  AgentUpdateMessageDelta,
			Delta: `{"verdict":"pass","score":0.93,"summary":"Acceptance criteria are covered.","findings":[{"severity":"p2","body":"Follow-up polish.","path":"README.md","line":12}]}`,
		}},
		result: AgentTurnResult{ThreadID: "validator-thread", TurnID: "validator-turn"},
	}
	codeBackend := &fakeCodexClient{}

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Gate: gate.Config{
					Validator: gate.ValidatorConfig{
						Enabled:       true,
						Model:         "gpt-5-validator-override",
						MinScore:      0.8,
						BlockOn:       []string{"p1"},
						TurnTimeoutMS: 120000,
					},
				},
				Agents: config.Agents{
					Backends: []config.AgentBackend{
						{ID: "codex-code", Kind: "codex", Protocol: "app-server", Command: "codex app-server"},
						{ID: "codex-validator", Kind: "codex", Protocol: "app-server", Command: "codex app-server --profile validator"},
					},
					Routes: []config.AgentRoute{
						{Name: "validator", Role: RoleValidator, Backend: "codex-validator", Model: "gpt-5-route-validator"},
						{Name: "default", Backend: "codex-code", Default: true},
					},
				},
			},
			Prompt: "Work on {{ issue.identifier }}",
		},
		Workspace: workspaceBackend,
		AgentBackends: map[string]AgentBackend{
			"codex-code":      codeBackend,
			"codex-validator": validatorBackend,
		},
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Validate(context.Background(), ValidatorRequest{
		Issue: connector.Issue{
			ID:          "issue-522",
			Identifier:  "digitaldrywood/detent#522",
			Title:       "Add validator gate",
			Description: "## Acceptance Criteria\n- Validator checks the PR diff.",
			PullRequest: &connector.PullRequest{
				URL:        "https://github.test/digitaldrywood/detent/pull/522",
				BranchName: "detent/digitaldrywood_detent_522",
				BaseSHA:    "base-sha",
			},
		},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if !result.Submitted || result.Verdict != gate.ValidatorVerdictPass || result.Score != 0.93 {
		t.Fatalf("Validate() result = %#v, want submitted pass score 0.93", result)
	}
	if result.Summary != "Acceptance criteria are covered." {
		t.Fatalf("Summary = %q, want parsed summary", result.Summary)
	}
	if len(result.Findings) != 1 || result.Findings[0].Severity != "p2" || result.Findings[0].Path != "README.md" || result.Findings[0].Line != 12 {
		t.Fatalf("Findings = %#v, want parsed p2 README finding", result.Findings)
	}
	if validatorBackend.request.Model != "gpt-5-validator-override" {
		t.Fatalf("validator model = %q, want gate override", validatorBackend.request.Model)
	}
	if validatorBackend.request.TurnTimeout != 2*time.Minute {
		t.Fatalf("validator turn timeout = %v, want 2m", validatorBackend.request.TurnTimeout)
	}
	if validatorBackend.request.Workspace != workspacePath {
		t.Fatalf("validator workspace = %q, want %q", validatorBackend.request.Workspace, workspacePath)
	}
	if workspaceBackend.createIssue.BaseRef != "base-sha" {
		t.Fatalf("workspace issue BaseRef = %q, want base-sha", workspaceBackend.createIssue.BaseRef)
	}
	for _, want := range []string{"validator-agent", "Acceptance Criteria", "git diff", "JSON"} {
		if !strings.Contains(validatorBackend.request.Prompt, want) {
			t.Fatalf("validator prompt missing %q:\n%s", want, validatorBackend.request.Prompt)
		}
	}
	if codeBackend.request.Prompt != "" {
		t.Fatalf("code backend prompt = %q, want unused code backend", codeBackend.request.Prompt)
	}
}

func TestRunnerUpdateWorkflowAppliesToFutureRuns(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-41", Branch: "detent/issue-41"},
	}
	codexClient := &fakeCodexClient{}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{},
			Prompt: "initial {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	runner.UpdateWorkflow(config.Workflow{
		Config: config.Config{},
		Prompt: "reloaded {{ issue.identifier }}",
	})

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-41",
			Identifier: "digitaldrywood/detent#41",
			Title:      "Reload workflow",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(codexClient.request.Prompt, "reloaded digitaldrywood/detent#41") {
		t.Fatalf("codex prompt = %q, want reloaded workflow prompt", codexClient.request.Prompt)
	}
}

func TestRunnerUpdateWorkflowRefreshesBudgetGuards(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 2, 11, 0, 0, 0, time.UTC)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-42", Branch: "detent/issue-42"},
	}
	agentBackend := &fakeCodexClient{}
	maxUSD := 2.0
	checker := &fakeBudgetChecker{
		refusal: budget.Refusal{
			Code:              budget.ReasonPerDayMaxUSD,
			Message:           "daily budget exceeded",
			Model:             "gpt-budget",
			CurrentSpendUSD:   1.90,
			ProjectedCostUSD:  0.20,
			ProjectedSpendUSD: 2.10,
			MaxUSD:            &maxUSD,
			RefusedAt:         now,
			CooldownUntil:     now.Add(time.Hour),
		},
	}
	estimator := &fakeDispatchEstimator{
		estimate: budget.TokenEstimate{
			InputTokens: 10,
			TotalTokens: 10,
			Sessions:    5,
		},
	}
	var guardCalls []bool
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{},
			Prompt: "initial {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		BudgetGuardBuilder: func(cfg config.Budget) (BudgetChecker, DispatchEstimator, error) {
			guardCalls = append(guardCalls, cfg.Enabled)
			if !cfg.Enabled {
				return nil, nil, nil
			}
			return checker, estimator, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	enabled := config.Config{}
	enabled.Budget.Enabled = true
	runner.UpdateWorkflow(config.Workflow{
		Config: enabled,
		Prompt: "reloaded {{ issue.identifier }}",
	})

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-42",
			Identifier:    "digitaldrywood/detent#42",
			ModelOverride: "gpt-budget",
		},
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(guardCalls) != 2 || guardCalls[0] || !guardCalls[1] {
		t.Fatalf("budget guard builder calls = %v, want [false true]", guardCalls)
	}
	if result.BudgetRefusal == nil || result.BudgetRefusal.Code != string(budget.ReasonPerDayMaxUSD) {
		t.Fatalf("BudgetRefusal = %#v, want daily budget refusal", result.BudgetRefusal)
	}
	if checker.calls != 1 || checker.model != "gpt-budget" {
		t.Fatalf("budget checker calls/model = %d/%q, want 1/gpt-budget", checker.calls, checker.model)
	}
	if estimator.model != "gpt-budget" {
		t.Fatalf("estimator model = %q, want gpt-budget", estimator.model)
	}
	if agentBackend.calls != 0 {
		t.Fatalf("RunTurn calls = %d, want 0 after budget refusal", agentBackend.calls)
	}
}

func TestRunnerRunUsesSingleConfiguredBackendDefaultRoute(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-55", Branch: "detent/issue-55"},
	}
	backend := &fakeCodexClient{}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agents: config.Agents{
					Backends: []config.AgentBackend{{
						ID:       "codex-high",
						Kind:     "codex",
						Protocol: "app-server",
						Command:  "codex app-server --profile high",
					}},
					Routes: []config.AgentRoute{{
						Backend: "codex-high",
						Default: true,
					}},
				},
			},
			Prompt: "work {{ issue.identifier }}",
		},
		Workspace: workspaceBackend,
		AgentBackends: map[string]AgentBackend{
			"codex-high": backend,
		},
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-55",
			Identifier:    "digitaldrywood/detent#55",
			ModelOverride: "gpt-5-codex-high",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if backend.request.Model != "gpt-5-codex-high" {
		t.Fatalf("Model = %q, want issue override", backend.request.Model)
	}
	if backend.request.Workspace != workspaceBackend.info.Path {
		t.Fatalf("Workspace = %q, want %q", backend.request.Workspace, workspaceBackend.info.Path)
	}
}

func TestRunnerRunRoutesAtMeSelectorsWithContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		issue   connector.Issue
		route   config.AgentRoute
		request RunRequest
		cfg     config.Config
	}{
		{
			name: "instance login",
			issue: connector.Issue{
				ID:         "issue-56",
				Identifier: "digitaldrywood/detent#56",
				Assignees:  []string{"worker-1"},
			},
			route: config.AgentRoute{
				Backend: "codex",
				Model:   "gpt-5-codex-high",
				Selector: selector.Selector{
					AssigneeIn: []string{"@me"},
				},
			},
			request: RunRequest{
				SelectorContext: selector.Context{InstanceLogin: "worker-1"},
			},
		},
		{
			name: "tracker assignee persona",
			issue: connector.Issue{
				ID:         "issue-57",
				Identifier: "digitaldrywood/detent#57",
				AuthorID:   "persona-reviewer",
			},
			route: config.AgentRoute{
				Backend: "codex",
				Model:   "gpt-5-codex-high",
				Selector: selector.Selector{
					AuthorIn: []string{"@me"},
				},
			},
			cfg: config.Config{
				Tracker: config.Tracker{Assignee: "persona-reviewer"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspaceBackend := &fakeWorkspaceBackend{
				info: workspace.Info{Path: t.TempDir(), Key: tt.issue.ID, Branch: "detent/" + tt.issue.ID},
			}
			backend := &fakeCodexClient{}
			cfg := tt.cfg
			cfg.Agents = config.Agents{
				Backends: []config.AgentBackend{{
					ID:       "codex",
					Kind:     "codex",
					Protocol: "app-server",
					Command:  "codex app-server",
				}},
				Routes: []config.AgentRoute{
					tt.route,
					{Backend: "codex", Model: "gpt-5-codex-mini", Default: true},
				},
			}
			runner, err := NewRunner(Dependencies{
				Workflow:     config.Workflow{Config: cfg, Prompt: "work {{ issue.identifier }}"},
				Workspace:    workspaceBackend,
				AgentBackend: backend,
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			req := tt.request
			req.Issue = tt.issue
			_, err = runner.Run(context.Background(), req)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if backend.request.Model != "gpt-5-codex-high" {
				t.Fatalf("Model = %q, want @me route model", backend.request.Model)
			}
		})
	}
}

func TestRunnerRunFinishesFailedSessionAndAfterRunOnCodexError(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-22", Branch: "detent/issue-22"},
	}
	codexClient := &fakeCodexClient{err: errors.New("codex failed")}
	sessionStore := &fakeSessionStore{sessionID: 7}
	now := newFakeClock(time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC))

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
		Store:        sessionStore,
		Now:          now.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-22",
			Identifier: "digitaldrywood/detent#22",
			Title:      "Add runner",
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want codex failure")
	}
	if !strings.Contains(err.Error(), "codex failed") {
		t.Fatalf("Run() error = %v, want codex failure", err)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun was not called after codex failure")
	}
	if workspaceBackend.diffed {
		t.Fatal("DiffStat was called after codex failure")
	}
	if sessionStore.finished.FinalState != FinalStateFailed {
		t.Fatalf("SessionFinish.FinalState = %q, want %q", sessionStore.finished.FinalState, FinalStateFailed)
	}
}

func TestRunnerRunRecordsFailedOutputTailNote(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: workspacePath, Key: "issue-856", Branch: "detent/issue-856"},
	}
	oldOutput := strings.Repeat("old output ", 2048)
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{{
			Type:   AgentUpdateMessageDelta,
			ItemID: "msg-1",
			Delta:  oldOutput + "useful failure tail",
		}},
		err: errors.New("codex failed"),
	}
	nowValue := time.Date(2026, 7, 2, 21, 50, 0, 0, time.UTC)
	now := newFakeClock(nowValue, nowValue, nowValue, nowValue, nowValue)

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
		Now:          now.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-856",
			Identifier: "digitaldrywood/detent#856",
			Title:      "Failure handoff",
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want codex failure")
	}

	notesPath, err := notes.WorkspacePath(workspacePath)
	if err != nil {
		t.Fatalf("notes path: %v", err)
	}
	content, err := notes.Read(notesPath, notes.ReadOptions{})
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	for _, want := range []string{
		"## 2026-07-02T21:50:00Z - Failed run output tail",
		"- final_state: failed",
		"- error: codex failed",
		"useful failure tail",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("notes missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, oldOutput) {
		t.Fatalf("notes included unbounded old output")
	}

	prompt, err := BuildPrompt(config.Workflow{Prompt: "Retry prompt"}, connector.Issue{
		Identifier: "digitaldrywood/detent#856",
	}, PromptOptions{WorkspacePath: workspacePath})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	if !strings.Contains(prompt, "useful failure tail") {
		t.Fatalf("retry prompt missing failure tail:\n%s", prompt)
	}
}

func TestRunnerRunTruncatesConfiguredAgentOutputTelemetry(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: workspacePath, Key: "issue-output", Branch: "detent/issue-output"},
	}
	largeOutput := "0123456789abcdefghijklmnopqrstuvwxyz"
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{{
			Type:     AgentUpdateMessageDelta,
			ThreadID: "thread-1",
			TurnID:   "turn-1",
			ItemID:   "msg-1",
			Delta:    largeOutput,
		}},
		result: AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1", SessionID: "thread-1-turn-1"},
	}
	workflowConfig := config.Default()
	workflowConfig.Agent.OutputTruncation.MaxBytes = len(runtimeoutput.Marker) + 10

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: workflowConfig, Prompt: "Prompt"},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	var usageUpdates []UsageUpdate
	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-output",
			Identifier: "digitaldrywood/detent#978",
			Title:      "Truncate runtime output",
		},
		OnUsageUpdate: func(update UsageUpdate) error {
			usageUpdates = append(usageUpdates, update)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantOutput := "01234" + runtimeoutput.Marker + "vwxyz"
	if result.Output != wantOutput {
		t.Fatalf("RunResult.Output = %q, want %q", result.Output, wantOutput)
	}
	if len(usageUpdates) == 0 {
		t.Fatal("OnUsageUpdate was not called")
	}
	last := usageUpdates[len(usageUpdates)-1]
	if last.LastMessage != wantOutput {
		t.Fatalf("UsageUpdate.LastMessage = %q, want %q", last.LastMessage, wantOutput)
	}
	if last.LastMessageTruncation == nil || !last.LastMessageTruncation.Truncated {
		t.Fatalf("UsageUpdate.LastMessageTruncation = %#v, want truncated metadata", last.LastMessageTruncation)
	}
	if last.LastMessageTruncation.OriginalBytes != len(largeOutput) {
		t.Fatalf("OriginalBytes = %d, want %d", last.LastMessageTruncation.OriginalBytes, len(largeOutput))
	}
	if len(last.RecentEvents) != 2 {
		t.Fatalf("RecentEvents length = %d, want route selection and message", len(last.RecentEvents))
	}
	if last.RecentEvents[1].Message != wantOutput {
		t.Fatalf("RecentEvents[1].Message = %q, want %q", last.RecentEvents[1].Message, wantOutput)
	}
	if last.RecentEvents[1].Truncation == nil || !last.RecentEvents[1].Truncation.Truncated {
		t.Fatalf("RecentEvents[1].Truncation = %#v, want truncated metadata", last.RecentEvents[1].Truncation)
	}
}

func TestRunnerRunTreatsMissingWorkspaceFinalDiffAsCompleted(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	startedAt := time.Date(2026, 6, 14, 15, 10, 0, 0, time.UTC)
	completedAt := startedAt.Add(4 * time.Second)
	workspaceBackend := &fakeWorkspaceBackend{
		info:    workspace.Info{Path: filepath.Join(t.TempDir(), "missing-worktree"), Key: "issue-453", Branch: "detent/issue-453"},
		diffErr: errors.Join(workspace.ErrMissingWorkspace, os.ErrNotExist),
	}
	codexClient := &fakeCodexClient{
		updates: []AgentUpdate{{
			Type:     AgentUpdateTokenUsage,
			ThreadID: "thread-453",
			TurnID:   "turn-1",
			Tokens: AgentTokenUsage{
				InputTokens:  100,
				OutputTokens: 25,
				TotalTokens:  125,
			},
		}},
		result: AgentTurnResult{ThreadID: "thread-453", TurnID: "turn-1", SessionID: "thread-453-turn-1"},
	}
	sessionStore := &fakeSessionStore{sessionID: 453}
	now := newFakeClock(
		startedAt,
		startedAt.Add(time.Second),
		completedAt,
		completedAt,
	)

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
		Store:        sessionStore,
		Now:          now.Now,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-453",
			Identifier: "digitaldrywood/detent#453",
			Title:      "Release snapshot",
		},
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want completed run despite missing workspace diff", err)
	}
	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateCompleted)
	}
	if result.DiffStats != (DiffStats{}) {
		t.Fatalf("DiffStats = %#v, want empty when workspace disappeared", result.DiffStats)
	}
	if sessionStore.finished.FinalState != FinalStateCompleted {
		t.Fatalf("SessionFinish.FinalState = %q, want %q", sessionStore.finished.FinalState, FinalStateCompleted)
	}
	logOutput := logs.String()
	for _, want := range []string{
		"workspace final diff stat skipped",
		"issue_id=issue-453",
		"issue_identifier=digitaldrywood/detent#453",
		"workspace_path=" + workspaceBackend.info.Path,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output missing %q:\n%s", want, logOutput)
		}
	}
}

func TestRunnerRunUsesFreshContextForAfterRunCleanup(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-22", Branch: "detent/issue-22"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	codexClient := &cancelingCodexClient{cancel: cancel}

	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(ctx, RunRequest{
		Issue: connector.Issue{
			ID:         "issue-22",
			Identifier: "digitaldrywood/detent#22",
			Title:      "Add runner",
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun was not called")
	}
	if workspaceBackend.afterRunErr != nil {
		t.Fatalf("AfterRun context error = %v, want nil", workspaceBackend.afterRunErr)
	}
}

func TestRunnerReapWorkspaceUsesWorkspaceIssueCleanup(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		cleanupResult: workspace.CleanupResult{Worktrees: 1, Branches: 1, Processes: 2},
	}
	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}},
		Workspace:    workspaceBackend,
		AgentBackend: &fakeCodexClient{},
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.ReapWorkspace(context.Background(), connector.Issue{
		ID:         "issue-311",
		Identifier: "digitaldrywood/detent#311",
		BranchName: "detent/digitaldrywood_detent_311",
	})
	if err != nil {
		t.Fatalf("ReapWorkspace() error = %v", err)
	}

	if result.Worktrees != 1 || result.Branches != 1 || result.Processes != 2 {
		t.Fatalf("ReapWorkspace() result = %#v, want 1 worktree, 1 branch, 2 processes", result)
	}
	if workspaceBackend.cleanupIssue.ProjectID != "default" ||
		workspaceBackend.cleanupIssue.ID != "issue-311" ||
		workspaceBackend.cleanupIssue.Identifier != "digitaldrywood/detent#311" ||
		workspaceBackend.cleanupIssue.BranchName != "detent/digitaldrywood_detent_311" {
		t.Fatalf("CleanupIssue() issue = %#v", workspaceBackend.cleanupIssue)
	}
}

type committingAgentBackend struct {
	request AgentTurnRequest
}

func (b *committingAgentBackend) RunTurn(ctx context.Context, req AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.request = req
	if err := os.WriteFile(filepath.Join(req.Workspace, "agent.txt"), []byte("agent edit\n"), 0o600); err != nil {
		return AgentTurnResult{}, fmt.Errorf("write agent edit: %w", err)
	}
	if err := runAgentGit(ctx, req.Workspace, "add", "agent.txt"); err != nil {
		return AgentTurnResult{}, err
	}
	if err := runAgentGit(ctx, req.Workspace, "commit", "-m", "agent commit"); err != nil {
		return AgentTurnResult{}, err
	}
	return AgentTurnResult{ThreadID: "thread-743", TurnID: "turn-1", SessionID: "thread-743-turn-1"}, nil
}

func runAgentGit(ctx context.Context, dir string, args ...string) error {
	gitArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s failed: %w\n%s", strings.Join(gitArgs, " "), err, output)
	}
	return nil
}

func initRunnerSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runRunnerGit(t, dir, "init", "-b", "main")
	runRunnerGit(t, dir, "config", "core.autocrlf", "false")
	runRunnerGit(t, dir, "config", "user.name", "Test User")
	runRunnerGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runRunnerGit(t, dir, "add", "README.md")
	runRunnerGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runRunnerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(cmd.Args[1:], " "), err, output)
	}
	return string(output)
}

func containsRunnerString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fakeWorkspaceBackend struct {
	info          workspace.Info
	diffStat      workspace.DiffStat
	diffStats     []workspace.DiffStat
	diffErr       error
	created       bool
	beforeRun     bool
	afterRun      bool
	afterRunErr   error
	diffed        bool
	diffCalls     int
	createIssue   workspace.Issue
	cleanupIssue  workspace.Issue
	cleanupResult workspace.CleanupResult
}

func (b *fakeWorkspaceBackend) Create(_ context.Context, issue workspace.Issue) (workspace.Info, error) {
	b.created = true
	b.createIssue = issue
	b.info.Branch = issue.BranchName
	return b.info, nil
}

func (b *fakeWorkspaceBackend) Cleanup(context.Context, string) error {
	return nil
}

func (b *fakeWorkspaceBackend) CleanupIssue(_ context.Context, issue workspace.Issue) (workspace.CleanupResult, error) {
	b.cleanupIssue = issue
	return b.cleanupResult, nil
}

func (b *fakeWorkspaceBackend) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	b.beforeRun = true
	return nil
}

func (b *fakeWorkspaceBackend) AfterRun(ctx context.Context, _ workspace.Info, _ workspace.Issue) {
	b.afterRun = true
	b.afterRunErr = ctx.Err()
}

func (b *fakeWorkspaceBackend) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	b.diffed = true
	if b.diffErr != nil {
		return workspace.DiffStat{}, b.diffErr
	}
	if len(b.diffStats) > 0 {
		index := b.diffCalls
		if index >= len(b.diffStats) {
			index = len(b.diffStats) - 1
		}
		b.diffCalls++
		return b.diffStats[index], nil
	}
	return b.diffStat, nil
}

type fakeMergeWorkspaceBackend struct {
	fakeWorkspaceBackend
	prepareResult workspace.MergePrepareResult
	prepareErr    error
	prepareCalled bool
}

func (b *fakeMergeWorkspaceBackend) PrepareMerge(
	context.Context,
	workspace.Info,
	workspace.Issue,
	workspace.MergePrepareOptions,
) (workspace.MergePrepareResult, error) {
	b.prepareCalled = true
	return b.prepareResult, b.prepareErr
}

type fakeCodexClient struct {
	request AgentTurnRequest
	updates []AgentUpdate
	result  AgentTurnResult
	err     error
	calls   int
}

func (c *fakeCodexClient) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	c.calls++
	c.request = req
	for _, update := range c.updates {
		if err := onUpdate(update); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return c.result, c.err
}

type resumeFallbackAgentBackend struct {
	requests []AgentTurnRequest
}

func (b *resumeFallbackAgentBackend) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	b.requests = append(b.requests, req)
	if !agentResumeEmpty(req.Resume) {
		return AgentTurnResult{}, errors.New("resume failed")
	}
	if onUpdate != nil {
		if err := onUpdate(AgentUpdate{
			Type:     AgentUpdateTokenUsage,
			ThreadID: "thread-fresh",
			TurnID:   "turn-fresh",
			Model:    "gpt-5-codex",
			Tokens: AgentTokenUsage{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
			},
		}); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{ThreadID: "thread-fresh", TurnID: "turn-fresh", SessionID: "session-fresh"}, nil
}

type resumeStartedFailureAgentBackend struct {
	requests []AgentTurnRequest
}

func (b *resumeStartedFailureAgentBackend) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	b.requests = append(b.requests, req)
	if onUpdate != nil {
		if err := onUpdate(AgentUpdate{
			Type:     AgentUpdateTurnStarted,
			ThreadID: "thread-old",
			TurnID:   "turn-old",
			Model:    "gpt-5-codex",
		}); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{ThreadID: "thread-old", TurnID: "turn-old", SessionID: "session-old"}, errors.New("resumed turn failed")
}

type cancelingCodexClient struct {
	cancel context.CancelFunc
}

func (c *cancelingCodexClient) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	c.cancel()
	return AgentTurnResult{}, context.Canceled
}

type fakeSessionStore struct {
	sessionID       int64
	started         store.SessionStart
	finished        store.SessionFinish
	usage           store.UsageEvent
	phase           store.WorkflowPhaseEvent
	identityUpdates []agentidentity.Identity
	startCalls      int
	finishCalls     int
	usageCalls      int
	resumeState     store.AgentResumeState
	resumeStates    map[string]store.AgentResumeState
	resumeErr       error
	resumeLookups   int
	resumeLookup    store.AgentResumeLookup
}

func (s *fakeSessionStore) StartSession(_ context.Context, attrs store.SessionStart) (int64, error) {
	s.startCalls++
	s.started = attrs
	return s.sessionID, nil
}

func (s *fakeSessionStore) FinishSession(_ context.Context, _ int64, attrs store.SessionFinish) error {
	s.finishCalls++
	s.finished = attrs
	return nil
}

func (s *fakeSessionStore) UpdateSessionIdentity(_ context.Context, _ int64, identity agentidentity.Identity) error {
	s.identityUpdates = append(s.identityUpdates, identity)
	return nil
}

func (s *fakeSessionStore) RecordUsageEvent(_ context.Context, attrs store.UsageEvent) (int64, error) {
	s.usageCalls++
	s.usage = attrs
	return 1, nil
}

func (s *fakeSessionStore) RecordWorkflowPhaseEvent(_ context.Context, attrs store.WorkflowPhaseEvent) (int64, error) {
	s.phase = attrs
	return 1, nil
}

type fakeDispatchEstimator struct {
	model    string
	estimate budget.TokenEstimate
	err      error
}

func (e *fakeDispatchEstimator) EstimateDispatch(_ context.Context, model string) (budget.TokenEstimate, error) {
	e.model = model
	return e.estimate, e.err
}

type fakeBudgetChecker struct {
	refusal budget.Refusal
	model   string
	calls   int
}

func (c *fakeBudgetChecker) CheckDispatch(_ context.Context, req budget.DispatchRequest) (budget.Decision, error) {
	c.calls++
	c.model = req.Model
	if c.refusal.Code == "" {
		return budget.Decision{Allowed: true}, nil
	}
	refusal := c.refusal
	return budget.Decision{Refusal: &refusal}, nil
}

type fakeRunnerBudgetSpendStore struct {
	daily      store.TokenSpend
	issue      store.TokenSpend
	dailyCalls int
	issueCalls int
}

func (s *fakeRunnerBudgetSpendStore) DailyTokenSpend(context.Context, time.Time) (store.TokenSpend, error) {
	s.dailyCalls++
	return s.daily, nil
}

func (s *fakeRunnerBudgetSpendStore) IssueTokenSpend(context.Context, store.IssueIdentity) (store.TokenSpend, error) {
	s.issueCalls++
	return s.issue, nil
}

func (s *fakeSessionStore) LatestCompletedAgentResumeState(_ context.Context, attrs store.AgentResumeLookup) (store.AgentResumeState, error) {
	s.resumeLookups++
	s.resumeLookup = attrs
	if s.resumeErr != nil {
		return store.AgentResumeState{}, s.resumeErr
	}
	if s.resumeStates != nil {
		state := s.resumeStates[attrs.AgentRole]
		if state.DetentSessionID == 0 && state.ProviderThreadID == "" && state.ProviderSessionID == "" {
			return store.AgentResumeState{}, store.ErrNotFound
		}
		return state, nil
	}
	if s.resumeState.DetentSessionID == 0 && s.resumeState.ProviderThreadID == "" && s.resumeState.ProviderSessionID == "" {
		return store.AgentResumeState{}, store.ErrNotFound
	}
	return s.resumeState, nil
}

func (s *fakeSessionStore) LatestIssueAgentResumeState(context.Context, store.IssueIdentity) (store.AgentResumeState, error) {
	return store.AgentResumeState{}, store.ErrNotFound
}

type fakeClock struct {
	values []time.Time
}

func newFakeClock(values ...time.Time) *fakeClock {
	return &fakeClock{values: values}
}

func (c *fakeClock) Now() time.Time {
	if len(c.values) == 0 {
		return time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC)
	}
	value := c.values[0]
	c.values = c.values[1:]
	return value
}

func writeSkill(t *testing.T, workspacePath, name, skillName, description, whenToUse string) {
	t.Helper()

	skillsDir := filepath.Join(workspacePath, ".detent", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	content := strings.Join([]string{
		"---",
		"name: " + skillName,
		"description: " + description,
		"when_to_use: " + whenToUse,
		"---",
		"Skill body stays out of the prompt.",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(skillsDir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}
