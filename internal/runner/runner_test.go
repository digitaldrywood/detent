package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
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
				Tokens: AgentTokenUsage{
					InputTokens:  100,
					OutputTokens: 25,
					TotalTokens:  125,
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
				Codex: config.Codex{
					ApprovalPolicy: config.StringValue("never"),
					ThreadSandbox:  "workspace-write",
					TurnSandboxPolicy: map[string]any{
						"type":          "workspaceWrite",
						"networkAccess": true,
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
				USDPerInputToken:  0.000004,
				USDPerOutputToken: 0.00002,
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
	if len(usageUpdates) != 3 {
		t.Fatalf("usage updates len = %d, want 3", len(usageUpdates))
	}
	if usageUpdates[0].SessionID != "thread-1-turn-1" || usageUpdates[0].TurnCount != 1 {
		t.Fatalf("first usage update = %#v, want live session and one turn", usageUpdates[0])
	}
	if usageUpdates[0].ProcessIdentity != "4242" {
		t.Fatalf("first usage update ProcessIdentity = %q, want 4242", usageUpdates[0].ProcessIdentity)
	}
	if usageUpdates[0].WorkspacePath != workspacePath {
		t.Fatalf("first usage update WorkspacePath = %q, want %q", usageUpdates[0].WorkspacePath, workspacePath)
	}
	if usageUpdates[0].LastEvent != "agent_message_delta" || usageUpdates[0].LastMessage != "hello" {
		t.Fatalf("first usage update activity = %#v, want agent message", usageUpdates[0])
	}
	if len(usageUpdates[0].RecentEvents) != 1 || usageUpdates[0].RecentEvents[0].Message != "hello" {
		t.Fatalf("first usage update RecentEvents = %#v, want latest agent message", usageUpdates[0].RecentEvents)
	}
	if usageUpdates[0].LastEventAt.IsZero() {
		t.Fatal("first usage update LastEventAt is zero")
	}
	if usageUpdates[0].DiffStats.FilesChanged != 1 || usageUpdates[0].DiffStats.AddedLines != 2 || usageUpdates[0].DiffStats.Status != "ok" {
		t.Fatalf("first usage update DiffStats = %#v, want live diff", usageUpdates[0].DiffStats)
	}
	if usageUpdates[1].TurnCount != 1 || usageUpdates[1].Tokens.TotalTokens != 125 {
		t.Fatalf("second usage update = %#v, want 1 turn and 125 tokens", usageUpdates[1])
	}
	if usageUpdates[1].Tokens.RuntimeSeconds != 2 {
		t.Fatalf("second usage update runtime = %v, want 2", usageUpdates[1].Tokens.RuntimeSeconds)
	}
	if len(usageUpdates[1].RecentEvents) != 2 || usageUpdates[1].RecentEvents[1].Event != "token_usage" || usageUpdates[1].RecentEvents[1].Message != "125 total tokens (100 in, 25 out)" {
		t.Fatalf("second usage update RecentEvents = %#v, want token-specific activity", usageUpdates[1].RecentEvents)
	}
	if usageUpdates[1].DiffStats.FilesChanged != 1 || usageUpdates[1].DiffStats.AddedLines != 2 || usageUpdates[1].DiffStats.RemovedLines != 0 {
		t.Fatalf("second usage update DiffStats = %#v, want cached diff", usageUpdates[1].DiffStats)
	}
	if usageUpdates[2].RateLimits == nil || usageUpdates[2].RateLimits.LimitID != "codex-primary" {
		t.Fatalf("third usage update RateLimits = %#v, want codex-primary", usageUpdates[2].RateLimits)
	}
	if len(usageUpdates[2].RecentEvents) != 3 || usageUpdates[2].RecentEvents[2].Event != "rate_limits" || usageUpdates[2].RecentEvents[2].Message != "Codex primary rate limits updated" {
		t.Fatalf("third usage update RecentEvents = %#v, want rate-limit-specific activity", usageUpdates[2].RecentEvents)
	}
	if usageUpdates[2].DiffStats.FilesChanged != 2 || usageUpdates[2].DiffStats.AddedLines != 5 || usageUpdates[2].DiffStats.RemovedLines != 1 {
		t.Fatalf("third usage update DiffStats = %#v, want refreshed diff", usageUpdates[2].DiffStats)
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
	if sessionStore.started.Identifier != "digitaldrywood/detent#22" || sessionStore.started.Model != "gpt-5-codex-high" {
		t.Fatalf("SessionStart = %#v, want issue identity and model", sessionStore.started)
	}
	if sessionStore.finished.FinalState != FinalStateCompleted || sessionStore.finished.TotalTokens != 125 || sessionStore.finished.Turns != 1 {
		t.Fatalf("SessionFinish = %#v, want completed session with tokens", sessionStore.finished)
	}
	if sessionStore.usage.ProjectID != "detent" || sessionStore.usage.SessionID != 42 {
		t.Fatalf("UsageEvent identity = %#v, want project detent and session 42", sessionStore.usage)
	}
	if sessionStore.usage.IssueID != "issue-22" || sessionStore.usage.Identifier != "digitaldrywood/detent#22" {
		t.Fatalf("UsageEvent issue = %#v, want issue-22/digitaldrywood/detent#22", sessionStore.usage)
	}
	if sessionStore.usage.Model != "gpt-5-codex-high" || sessionStore.usage.TotalTokens != 125 {
		t.Fatalf("UsageEvent totals = %#v, want model gpt-5-codex-high and total 125", sessionStore.usage)
	}
	if sessionStore.usage.CostUSD != 0.0009 {
		t.Fatalf("UsageEvent CostUSD = %.12f, want 0.000900000000", sessionStore.usage.CostUSD)
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
	if sessionStore.phase.PhaseType != store.WorkflowPhaseTypeAgentSession || sessionStore.phase.PhaseName != "agent_active" || sessionStore.phase.Status != FinalStateCompleted {
		t.Fatalf("WorkflowPhaseEvent phase = %#v, want completed agent_active session", sessionStore.phase)
	}
	if sessionStore.phase.StartedAt != startedAt || sessionStore.phase.FinishedAt != completedAt || sessionStore.phase.DurationSeconds != 4 {
		t.Fatalf("WorkflowPhaseEvent timing = %#v, want 4s session", sessionStore.phase)
	}
	if sessionStore.phase.Turns != 1 || sessionStore.phase.InputTokens != 100 || sessionStore.phase.OutputTokens != 25 || sessionStore.phase.TotalTokens != 125 {
		t.Fatalf("WorkflowPhaseEvent usage = %#v, want turns and token totals", sessionStore.phase)
	}
}

func TestRunnerRunAddsGitMetadataWritableRootsForManagedWorkspace(t *testing.T) {
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
			Config: config.Config{
				Codex: config.Codex{
					ThreadSandbox: "workspace-write",
					TurnSandboxPolicy: map[string]any{
						"type":          "workspaceWrite",
						"networkAccess": true,
					},
				},
			},
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
	gotRoots := sandboxPolicyWritableRoots(t, agentBackend.request.TurnSandboxPolicy)
	for _, want := range wantRoots {
		if !containsRunnerString(gotRoots, want) {
			t.Fatalf("sandbox writableRoots = %#v, missing %q", gotRoots, want)
		}
	}
	policy := sandboxPolicyMapForTest(t, agentBackend.request.TurnSandboxPolicy)
	if policy["networkAccess"] != true {
		t.Fatalf("sandboxPolicy.networkAccess = %#v, want true", policy["networkAccess"])
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
			{Type: AgentUpdateTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"},
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
		"worker_process_started",
		"worker_turn_started",
		"worker_usage_updated",
		"worker_turn_finished",
		"worker_command_finished",
		"worker_after_run_finished",
		"worker_session_finished",
		"issue_id=issue-726",
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
			Config: config.Config{
				Codex: config.Codex{ThreadSandbox: "workspace-write"},
			},
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

func TestRunnerUsageCostWarnsForUnknownModel(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runner := &Runner{
		pricing: budget.PricingTable{},
		logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	}

	cost := runner.usageCostUSD(" missing-model ", 10, 5)
	if cost != 0 {
		t.Fatalf("usageCostUSD() = %.12f, want 0", cost)
	}
	if got := logs.String(); !strings.Contains(got, "usage event model pricing not found") || !strings.Contains(got, "missing-model") {
		t.Fatalf("log output = %q, want unknown pricing warning", got)
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

	cost := runner.usageCostUSD(" \t", 10, 5)
	if cost != 0 {
		t.Fatalf("usageCostUSD() = %.12f, want 0", cost)
	}
	got := logs.String()
	if strings.Contains(got, "level=WARN") || strings.Contains(got, "usage event model pricing not found") {
		t.Fatalf("log output = %q, want no empty-model pricing warning", got)
	}
	if !strings.Contains(got, "usage event model unavailable; skipping cost pricing") {
		t.Fatalf("log output = %q, want empty-model diagnostic", got)
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
						Enabled:  true,
						Model:    "gpt-5-validator-override",
						MinScore: 0.8,
						BlockOn:  []string{"p1"},
					},
				},
				Agents: config.Agents{
					Backends: []config.AgentBackend{
						{ID: "codex-code", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "codex app-server"},
						{ID: "codex-validator", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "codex app-server --profile validator"},
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
	if validatorBackend.request.Workspace != workspacePath {
		t.Fatalf("validator workspace = %q, want %q", validatorBackend.request.Workspace, workspacePath)
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
			Config: config.Config{
				Codex: config.Codex{ThreadSandbox: "workspace-write"},
			},
			Prompt: "initial {{ issue.identifier }}",
		},
		Workspace:    workspaceBackend,
		AgentBackend: codexClient,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	runner.UpdateWorkflow(config.Workflow{
		Config: config.Config{
			Codex: config.Codex{ThreadSandbox: "danger-full-access"},
		},
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
	if codexClient.request.ThreadSandbox != "danger-full-access" {
		t.Fatalf("ThreadSandbox = %q, want danger-full-access", codexClient.request.ThreadSandbox)
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
						Kind:     config.AgentBackendCodex,
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
					Kind:     config.AgentBackendCodex,
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

func sandboxPolicyWritableRoots(t *testing.T, policy any) []string {
	t.Helper()

	policyMap := sandboxPolicyMapForTest(t, policy)
	values, ok := policyMap["writableRoots"].([]string)
	if ok {
		return values
	}
	rawValues, ok := policyMap["writableRoots"].([]any)
	if !ok {
		t.Fatalf("sandboxPolicy.writableRoots = %#v, want string list", policyMap["writableRoots"])
	}
	roots := make([]string, 0, len(rawValues))
	for _, raw := range rawValues {
		root, ok := raw.(string)
		if !ok {
			t.Fatalf("sandboxPolicy.writableRoots item = %#v, want string", raw)
		}
		roots = append(roots, root)
	}
	return roots
}

func sandboxPolicyMapForTest(t *testing.T, policy any) map[string]any {
	t.Helper()

	policyMap, ok := policy.(map[string]any)
	if !ok {
		t.Fatalf("TurnSandboxPolicy = %T, want map[string]any", policy)
	}
	return policyMap
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

type fakeCodexClient struct {
	request AgentTurnRequest
	updates []AgentUpdate
	result  AgentTurnResult
	err     error
}

func (c *fakeCodexClient) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	c.request = req
	for _, update := range c.updates {
		if err := onUpdate(update); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return c.result, c.err
}

type cancelingCodexClient struct {
	cancel context.CancelFunc
}

func (c *cancelingCodexClient) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	c.cancel()
	return AgentTurnResult{}, context.Canceled
}

type fakeSessionStore struct {
	sessionID int64
	started   store.SessionStart
	finished  store.SessionFinish
	usage     store.UsageEvent
	phase     store.WorkflowPhaseEvent
}

func (s *fakeSessionStore) StartSession(_ context.Context, attrs store.SessionStart) (int64, error) {
	s.started = attrs
	return s.sessionID, nil
}

func (s *fakeSessionStore) FinishSession(_ context.Context, _ int64, attrs store.SessionFinish) error {
	s.finished = attrs
	return nil
}

func (s *fakeSessionStore) RecordUsageEvent(_ context.Context, attrs store.UsageEvent) (int64, error) {
	s.usage = attrs
	return 1, nil
}

func (s *fakeSessionStore) RecordWorkflowPhaseEvent(_ context.Context, attrs store.WorkflowPhaseEvent) (int64, error) {
	s.phase = attrs
	return 1, nil
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
