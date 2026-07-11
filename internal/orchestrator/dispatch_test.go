package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestHandleRunUpdatePersistsRuntimeIdentityHeartbeat(t *testing.T) {
	t.Parallel()

	attempts := &recordingWorkAttemptStore{}
	o := &Orchestrator{cfg: normalizeConfig(Config{}), workAttempts: attempts}
	state := newState(normalizeConfig(Config{}))
	issue := connector.Issue{ID: "issue-1118", Identifier: "digitaldrywood/detent#1118", State: "In Progress"}
	state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 42}
	state.WorkAttempts = []telemetry.WorkAttempt{{AttemptID: 42}}
	identity := agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
		Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{}))
	at := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)

	o.handleRunUpdate(&state, runUpdate{
		issueID: issue.ID,
		usage: runpkg.UsageUpdate{
			DetentSessionID: 1118,
			SessionID:       "thread-1118-turn-1",
			LastEventAt:     at,
			LastEvent:       "runtime_identity",
			RuntimeIdentity: identity,
		},
	})

	if len(attempts.heartbeats) != 1 {
		t.Fatalf("heartbeats = %#v, want immediate durable identity heartbeat", attempts.heartbeats)
	}
	heartbeat := attempts.heartbeats[0]
	if heartbeat.DetentSessionID != 1118 || heartbeat.ProviderSessionID != "thread-1118-turn-1" || !heartbeat.RuntimeIdentity.MateriallyEqual(identity) {
		t.Fatalf("heartbeat = %#v, want correlated runtime identity", heartbeat)
	}
	if !state.WorkAttempts[0].RuntimeIdentity.MateriallyEqual(identity) {
		t.Fatalf("snapshot work attempt identity = %#v, want runtime identity", state.WorkAttempts[0].RuntimeIdentity)
	}
}

func TestRecoverDurableWorkAttemptsRestoresMostRecentRuntimeIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 22, 0, 0, 0, time.UTC)
	issue := connector.Issue{ID: "issue-1118", Identifier: "digitaldrywood/detent#1118", State: "Human Review"}
	newest := agentidentity.Configured("claude-local", "claude_code", "local", "validator", "fable", "ollama", "high", "", now.Add(-time.Hour)).
		Merge(agentidentity.RuntimeUpdate("qwen3-coder", "", "", "", now.Add(-time.Hour)))
	older := agentidentity.Configured("codex-old", "codex", "default", "code", "gpt-5.5", "", "", "", now.Add(-2*time.Hour))
	attempts := &recordingWorkAttemptStore{recent: []store.WorkAttempt{
		{ID: 2, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Status: store.WorkAttemptStatusTerminal, RuntimeIdentity: newest},
		{ID: 1, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Status: store.WorkAttemptStatusTerminal, RuntimeIdentity: older},
	}}
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}})
	o := &Orchestrator{cfg: cfg, workAttempts: attempts}
	state := newState(cfg)
	state.BoardIssues = []connector.Issue{issue}

	o.recoverDurableWorkAttempts(context.Background(), &state, now)

	if len(state.WorkAttempts) != 2 || state.WorkAttempts[0].AttemptID != 2 || state.WorkAttempts[1].AttemptID != 1 {
		t.Fatalf("recovered attempts = %#v, want newest first", state.WorkAttempts)
	}
	snapshot := state.Snapshot(now)
	if len(snapshot.BoardIssues) != 1 || !snapshot.BoardIssues[0].RuntimeIdentity.MateriallyEqual(newest) {
		t.Fatalf("recovered board identity = %#v, want newest persisted identity", snapshot.BoardIssues)
	}
}

func TestConfigFromWorkflowIncludesDispatchControls(t *testing.T) {
	t.Parallel()

	perHost := 2
	cfg := workflowconfig.Default()
	cfg.Worker.SSHHosts = []string{"worker-a", "worker-b"}
	cfg.Worker.MaxConcurrentAgentsPerHost = &perHost
	cfg.Workspace.CleanupIdleTTLMS = 7200000
	cfg.Workspace.CleanupSweepIntervalMS = 120000
	cfg.Budget.RefusalCooldownSeconds = 45
	cfg.Agent.AutoPromote.Enabled = true
	cfg.Agent.AutoPromote.QuietSeconds = 30
	cfg.Agent.AutoPromote.OptoutLabel = " Requires-Human-Review "
	cfg.Agent.AutoPromote.AllowedIssueLabels = []string{" Docs ", "docs", "Chore"}
	cfg.Agent.AutoPromote.GateWaitState = " Review "
	cfg.Agent.AutoPromote.GateWaitTimeoutSeconds = 900
	cfg.Agent.AutoPromote.ReworkLimit = 2
	cfg.Agent.MergeFastPath.Enabled = true
	cfg.Agent.OutputTruncation.MaxBytes = 4096
	cfg.Identity.Name = "release-captain"
	cfg.Identity.GitHubLogin = "detent-bot"
	cfg.Tracker.Authorization = selector.Selector{
		AssigneeIn: []string{"@me"},
	}
	cfg.Tracker.Claims.Enabled = true
	cfg.Tracker.Claims.LeaseField = "Detent Lease"
	cfg.Tracker.Claims.TTLSeconds = 300
	cfg.Tracker.Claims.HeartbeatSeconds = 45
	cfg.Gate = gate.Config{Kind: gate.KindHumanReview, ApprovalLabel: " Approved-By-Human "}

	got := ConfigFromWorkflow(cfg)

	if got.MaxConcurrentAgentsPerHost != 2 {
		t.Fatalf("MaxConcurrentAgentsPerHost = %d, want 2", got.MaxConcurrentAgentsPerHost)
	}
	if len(got.WorkerHosts) != 2 || got.WorkerHosts[0] != "worker-a" || got.WorkerHosts[1] != "worker-b" {
		t.Fatalf("WorkerHosts = %#v, want worker-a and worker-b", got.WorkerHosts)
	}
	if got.BudgetRefusalCooldown != 45*time.Second {
		t.Fatalf("BudgetRefusalCooldown = %s, want 45s", got.BudgetRefusalCooldown)
	}
	if got.WorkspaceCleanupIdleTTL != 2*time.Hour {
		t.Fatalf("WorkspaceCleanupIdleTTL = %s, want 2h0m0s", got.WorkspaceCleanupIdleTTL)
	}
	if got.WorkspaceCleanupSweepInterval != 2*time.Minute {
		t.Fatalf("WorkspaceCleanupSweepInterval = %s, want 2m0s", got.WorkspaceCleanupSweepInterval)
	}
	if !got.AutoPromote.Enabled {
		t.Fatal("AutoPromote.Enabled = false, want true")
	}
	if got.AutoPromote.QuietDuration != 30*time.Second {
		t.Fatalf("AutoPromote.QuietDuration = %s, want 30s", got.AutoPromote.QuietDuration)
	}
	if got.AutoPromote.OptoutLabel != "requires-human-review" {
		t.Fatalf("AutoPromote.OptoutLabel = %q, want requires-human-review", got.AutoPromote.OptoutLabel)
	}
	if len(got.AutoPromote.AllowedIssueLabels) != 2 ||
		got.AutoPromote.AllowedIssueLabels[0] != "docs" ||
		got.AutoPromote.AllowedIssueLabels[1] != "chore" {
		t.Fatalf("AutoPromote.AllowedIssueLabels = %#v, want docs and chore", got.AutoPromote.AllowedIssueLabels)
	}
	if got.AutoPromote.GateWaitState != autoPromoteGateWaitReview {
		t.Fatalf("AutoPromote.GateWaitState = %q, want review", got.AutoPromote.GateWaitState)
	}
	if got.AutoPromote.GateWaitTimeout != 15*time.Minute {
		t.Fatalf("AutoPromote.GateWaitTimeout = %s, want 15m0s", got.AutoPromote.GateWaitTimeout)
	}
	if got.AutoPromote.ReworkLimit != 2 {
		t.Fatalf("AutoPromote.ReworkLimit = %d, want 2", got.AutoPromote.ReworkLimit)
	}
	if !got.MergeFastPathEnabled {
		t.Fatal("MergeFastPathEnabled = false, want true")
	}
	if got.OutputTruncationMaxBytes != 4096 {
		t.Fatalf("OutputTruncationMaxBytes = %d, want 4096", got.OutputTruncationMaxBytes)
	}
	if got.SelectorContext.InstanceLogin != "detent-bot" {
		t.Fatalf("SelectorContext.InstanceLogin = %q, want detent-bot", got.SelectorContext.InstanceLogin)
	}
	if got.SelectorContext.Persona != "release-captain" {
		t.Fatalf("SelectorContext.Persona = %q, want release-captain", got.SelectorContext.Persona)
	}
	if len(got.Authorization.AssigneeIn) != 1 || got.Authorization.AssigneeIn[0] != "@me" {
		t.Fatalf("Authorization.AssigneeIn = %#v, want @me", got.Authorization.AssigneeIn)
	}
	if !got.Claiming.Enabled {
		t.Fatal("Claiming.Enabled = false, want true")
	}
	if got.Claiming.OwnershipMode != workflowconfig.IdentityOwnershipAssignee {
		t.Fatalf("Claiming.OwnershipMode = %q, want assignee", got.Claiming.OwnershipMode)
	}
	if got.Claiming.Owner != "release-captain" {
		t.Fatalf("Claiming.Owner = %q, want release-captain", got.Claiming.Owner)
	}
	if got.Claiming.AssigneeLogin != "detent-bot" {
		t.Fatalf("Claiming.AssigneeLogin = %q, want detent-bot", got.Claiming.AssigneeLogin)
	}
	if got.Claiming.LeaseField != "Detent Lease" {
		t.Fatalf("Claiming.LeaseField = %q, want Detent Lease", got.Claiming.LeaseField)
	}
	if got.Claiming.LeaseTTL != 300*time.Second {
		t.Fatalf("Claiming.LeaseTTL = %s, want 5m0s", got.Claiming.LeaseTTL)
	}
	if got.Claiming.HeartbeatInterval != 45*time.Second {
		t.Fatalf("Claiming.HeartbeatInterval = %s, want 45s", got.Claiming.HeartbeatInterval)
	}
	if got.AutoPromote.Gate.Kind != gate.KindHumanReview || got.AutoPromote.Gate.ApprovalLabel != "approved-by-human" {
		t.Fatalf("AutoPromote.Gate = %#v, want human_review approved-by-human", got.AutoPromote.Gate)
	}
}

func TestDispatchableFiltersIneligibleCandidates(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    2,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled"},
		BudgetRefusalCooldown:  time.Hour,
		ContinuationRetryDelay: time.Second,
	})
	orch := Orchestrator{cfg: cfg}

	tests := []struct {
		name  string
		issue connector.Issue
		state func(State)
		want  bool
	}{
		{
			name:  "active issue",
			issue: dispatchTestIssue("issue-active", "Todo"),
			want:  true,
		},
		{
			name:  "terminal issue",
			issue: dispatchTestIssue("issue-terminal", "Done"),
			want:  false,
		},
		{
			name:  "inactive issue",
			issue: dispatchTestIssue("issue-inactive", "Backlog"),
			want:  false,
		},
		{
			name: "todo blocked by non-terminal dependency",
			issue: func() connector.Issue {
				issue := dispatchTestIssue("issue-blocked-dependency", "Todo")
				issue.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#10", State: "In Progress"}}
				return issue
			}(),
			want: false,
		},
		{
			name: "todo unblocked by terminal dependency",
			issue: func() connector.Issue {
				issue := dispatchTestIssue("issue-terminal-dependency", "Todo")
				issue.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#10", State: "Done"}}
				return issue
			}(),
			want: true,
		},
		{
			name: "todo unblocked by unknown dependency state",
			issue: func() connector.Issue {
				issue := dispatchTestIssue("issue-unknown-dependency", "Todo")
				issue.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#10"}}
				return issue
			}(),
			want: true,
		},
		{
			name:  "already running",
			issue: dispatchTestIssue("issue-running", "Todo"),
			state: func(state State) {
				issue := dispatchTestIssue("issue-running", "Todo")
				state.Running[issue.ID] = Running{Issue: issue}
			},
			want: false,
		},
		{
			name:  "already claimed",
			issue: dispatchTestIssue("issue-claimed", "Todo"),
			state: func(state State) {
				issue := dispatchTestIssue("issue-claimed", "Todo")
				state.Claimed[issue.ID] = Claimed{Issue: issue}
			},
			want: false,
		},
		{
			name:  "already blocked",
			issue: dispatchTestIssue("issue-blocked", "Todo"),
			state: func(state State) {
				issue := dispatchTestIssue("issue-blocked", "Todo")
				state.Blocked[issue.ID] = Blocked{Issue: issue}
			},
			want: false,
		},
		{
			name:  "budget cooldown active",
			issue: dispatchTestIssue("issue-budget", "Todo"),
			state: func(state State) {
				issue := dispatchTestIssue("issue-budget", "Todo")
				state.BudgetRefusals[issue.ID] = BudgetRefusal{
					Issue:     issue,
					RefusedAt: now.Add(-time.Minute),
				}
			},
			want: false,
		},
		{
			name:  "budget cooldown expired",
			issue: dispatchTestIssue("issue-budget-expired", "Todo"),
			state: func(state State) {
				issue := dispatchTestIssue("issue-budget-expired", "Todo")
				state.BudgetRefusals[issue.ID] = BudgetRefusal{
					Issue:     issue,
					RefusedAt: now.Add(-2 * time.Hour),
				}
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newState(cfg)
			if tt.state != nil {
				tt.state(state)
			}

			got := orch.dispatchable(tt.issue, &state, now)
			if got != tt.want {
				t.Fatalf("dispatchable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPruneBudgetRefusalsReevaluatesDailyCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	resetAt := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		refusal    BudgetRefusal
		status     DailyBudgetStatus
		statusErr  error
		wantActive bool
	}{
		{
			name: "cap raise clears resolved daily refusal",
			refusal: BudgetRefusal{
				Code:             "per_day_max_usd",
				ProjectedCostUSD: 10,
				ResetAt:          &resetAt,
				RefusedAt:        now,
			},
			status: DailyBudgetStatus{Active: true, CurrentSpendUSD: 100, MaxUSD: 250},
		},
		{
			name: "cap raise keeps daily refusal when projected spend remains over cap",
			refusal: BudgetRefusal{
				Code:             "per_day_max_usd",
				ProjectedCostUSD: 10,
				ResetAt:          &resetAt,
				RefusedAt:        now,
			},
			status:     DailyBudgetStatus{Active: true, CurrentSpendUSD: 245, MaxUSD: 250},
			wantActive: true,
		},
		{
			name: "unchanged cap keeps daily refusal until midnight",
			refusal: BudgetRefusal{
				Code:             "per_day_max_usd",
				ProjectedCostUSD: 10,
				ResetAt:          &resetAt,
				RefusedAt:        now,
			},
			status:     DailyBudgetStatus{Active: true, CurrentSpendUSD: 100, MaxUSD: 100},
			wantActive: true,
		},
		{
			name: "disabled daily cap clears daily refusal",
			refusal: BudgetRefusal{
				Code:             "per_day_max_usd",
				ProjectedCostUSD: 10,
				ResetAt:          &resetAt,
				RefusedAt:        now,
			},
		},
		{
			name: "status lookup failure preserves daily refusal",
			refusal: BudgetRefusal{
				Code:             "per_day_max_usd",
				ProjectedCostUSD: 10,
				ResetAt:          &resetAt,
				RefusedAt:        now,
			},
			statusErr:  errors.New("lookup failed"),
			wantActive: true,
		},
		{
			name: "per issue refusal keeps cooldown behavior",
			refusal: BudgetRefusal{
				Code:      "per_issue_max_usd",
				RefusedAt: now.Add(-30 * time.Minute),
			},
			status:     DailyBudgetStatus{Active: true, CurrentSpendUSD: 0, MaxUSD: 1000},
			wantActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{BudgetRefusalCooldown: time.Hour})
			state := newState(cfg)
			state.BudgetRefusals["issue"] = tt.refusal
			orch := Orchestrator{
				cfg: cfg,
				dailyBudgetStatus: fakeDailyBudgetStatusProvider{
					status: tt.status,
					err:    tt.statusErr,
				},
			}

			orch.dispatchTickIssues(
				context.Background(),
				&state,
				tickFetchedIssues{statusOK: true},
				tickTransitionRefresh{blockedRefreshOK: true},
				tickPreviousState{},
				nil,
				now,
			)
			_, gotActive := state.BudgetRefusals["issue"]
			if gotActive != tt.wantActive {
				t.Fatalf("budget refusal active = %t, want %t", gotActive, tt.wantActive)
			}
		})
	}
}

type fakeDailyBudgetStatusProvider struct {
	status DailyBudgetStatus
	err    error
}

func (p fakeDailyBudgetStatusProvider) DailyBudgetStatus(context.Context, time.Time) (DailyBudgetStatus, bool, error) {
	return p.status, true, p.err
}

func TestDispatchableSkipsAutoPromoteGatePendingActiveIssue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 13, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := dispatchTestIssueWithPullRequest("issue-gate-pending", "In Progress", "OPEN")
	issue.PullRequest.CIStatus = "pending"
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted}
	orch := Orchestrator{cfg: cfg}

	decision := orch.dispatchPlanner().dispatchableIssueDecision(issue, &state, false, now, "")
	if decision.dispatchable {
		t.Fatal("dispatchable gate-pending active issue = true, want false")
	}
	if decision.reason != dispatchSkipAwaitingGate {
		t.Fatalf("dispatchable reason = %q, want %q", decision.reason, dispatchSkipAwaitingGate)
	}
}

func TestDispatchPlannerSkipsCompletedGateWaitRetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 18, 44, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			GateWaitState: autoPromoteGateWaitSource,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := dispatchTestIssueWithPullRequest("issue-completed-gate-wait", "In Progress", "OPEN")
	issue.PullRequest.CIStatus = "pending"
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-3 * time.Minute),
		FinalState:  FinalStateCompleted,
	}
	planner := newDispatchPlanner(cfg)
	planner.scheduleRetryAfter(&state, issue, 1, now, 0, "", "")
	var decisions []dispatchPlanDecision

	plan := planner.plan(&state, []connector.Issue{issue}, now, dispatchPlanHooks{
		decision: func(decision dispatchPlanDecision) {
			decisions = append(decisions, decision)
		},
	})

	if len(plan.Dispatches) != 0 {
		t.Fatalf("dispatches = %#v, want completed gate-wait retry skipped", plan.Dispatches)
	}
	if len(decisions) != 1 || decisions[0].SkipReason != "awaiting_gate" {
		t.Fatalf("decisions = %#v, want awaiting_gate skip", decisions)
	}
	if _, ok := state.Completed[issue.ID]; !ok {
		t.Fatalf("Completed[%q] missing after gate-wait skip", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after gate-wait skip", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after gate-wait skip", issue.ID)
	}

	attempts := &recordingWorkAttemptStore{}
	orch := Orchestrator{cfg: cfg, workAttempts: attempts}
	loggedState := newState(cfg)
	loggedState.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-3 * time.Minute),
		FinalState:  FinalStateCompleted,
	}
	orch.dispatchReadyIssues(t.Context(), &loggedState, []connector.Issue{issue}, now)
	if len(attempts.decisions) != 1 || attempts.decisions[0].Reason != dispatchSkipAwaitingGate {
		t.Fatalf("scheduler decisions = %#v, want awaiting_gate skip", attempts.decisions)
	}
}

func TestDispatchableSkipsQuietWindowActiveIssueWithOpenPullRequest(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 13, 5, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := dispatchTestIssueWithPullRequest("issue-quiet-gate-pending", "In Progress", "OPEN")
	issue.PullRequest.CIStatus = "pending"
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted}
	orch := Orchestrator{cfg: cfg}

	decision := orch.dispatchPlanner().dispatchableIssueDecision(issue, &state, false, now, "")
	if decision.dispatchable {
		t.Fatal("dispatchable quiet-window active issue with open PR = true, want false")
	}
	if decision.reason != dispatchSkipAwaitingGate {
		t.Fatalf("dispatchable reason = %q, want %q", decision.reason, dispatchSkipAwaitingGate)
	}
}

func TestDispatchableArtifactGateWaitStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 15, 40, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate: gate.Config{
				Kind: gate.KindArtifact,
				Artifact: gate.ArtifactConfig{
					StatusField:    "render_status",
					PassStatuses:   []string{"approved", "valid"},
					WaitStatuses:   []string{"queued", "rendering", "pending_review"},
					ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
				},
			},
		},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	tests := []struct {
		name           string
		state          string
		status         string
		stageUpdatedAt *time.Time
		updatedAt      *time.Time
		fieldUpdatedAt *time.Time
		wantDispatch   bool
		wantReason     string
	}{
		{
			name:         "fresh item dispatches",
			state:        "Todo",
			wantDispatch: true,
		},
		{
			name:           "mid wait item stays skipped",
			state:          "Production",
			status:         "pending_review",
			stageUpdatedAt: timePointer(now.Add(-2 * time.Minute)),
			updatedAt:      timePointer(now.Add(-time.Minute)),
			fieldUpdatedAt: timePointer(now.Add(-time.Minute)),
			wantReason:     dispatchSkipArtifactGateWaitStatus,
		},
		{
			name:           "human restarted round dispatches",
			state:          "Todo",
			status:         "pending_review",
			stageUpdatedAt: timePointer(now),
			updatedAt:      timePointer(now),
			fieldUpdatedAt: timePointer(now.Add(-time.Minute)),
			wantDispatch:   true,
		},
		{
			name:           "newer state wins stale status race",
			state:          "Rework",
			status:         "pending_review",
			stageUpdatedAt: timePointer(now),
			updatedAt:      timePointer(now),
			fieldUpdatedAt: timePointer(now.Add(-time.Nanosecond)),
			wantDispatch:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dispatchTestIssue("issue-artifact-status", tt.state)
			issue.Fields = map[string]string{"render_status": tt.status}
			issue.StageUpdatedAt = tt.stageUpdatedAt
			issue.UpdatedAt = tt.updatedAt
			if tt.fieldUpdatedAt != nil {
				issue.FieldUpdatedAt = map[string]time.Time{"render_status": *tt.fieldUpdatedAt}
			}
			state := newState(cfg)
			orch := Orchestrator{cfg: cfg}

			decision := orch.dispatchPlanner().dispatchableIssueDecision(issue, &state, false, now, "")
			if decision.dispatchable != tt.wantDispatch {
				t.Fatalf("dispatchable = %t, want %t", decision.dispatchable, tt.wantDispatch)
			}
			if decision.reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", decision.reason, tt.wantReason)
			}
		})
	}

	issue := dispatchTestIssue("issue-artifact-pending-review", "Production")
	issue.Fields = map[string]string{"render_status": "pending_review"}
	issue.StageUpdatedAt = timePointer(now.Add(-2 * time.Minute))
	issue.UpdatedAt = timePointer(now.Add(-time.Minute))
	issue.FieldUpdatedAt = map[string]time.Time{"render_status": now.Add(-time.Minute)}
	state := newState(cfg)
	orch := Orchestrator{cfg: cfg}
	attempts := &recordingWorkAttemptStore{}
	orch.workAttempts = attempts
	orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
	if len(attempts.decisions) != 1 {
		t.Fatalf("scheduler decisions len = %d, want 1", len(attempts.decisions))
	}
	if got := attempts.decisions[0].Reason; got != dispatchSkipArtifactGateWaitStatus {
		t.Fatalf("scheduler decision reason = %q, want %q", got, dispatchSkipArtifactGateWaitStatus)
	}
}

func TestHandleRunResultRecordsBudgetRefusalAndComment(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    1,
		ActiveStates:           []string{"Todo"},
		TerminalStates:         []string{"Done"},
		BudgetRefusalCooldown:  time.Hour,
		ContinuationRetryDelay: time.Second,
	})
	commentConnector := &budgetRefusalCommentConnector{}
	orch := Orchestrator{
		cfg:       cfg,
		connector: commentConnector,
	}
	state := newState(cfg)
	issue := dispatchTestIssue("issue-budget-refused", "Todo")
	state.Running[issue.ID] = Running{
		Issue:      issue,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "local",
	}
	maxUSD := 2.5

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			BudgetRefusal: &runpkg.BudgetRefusal{
				Code:             "per_day_max_usd",
				Message:          "daily budget exceeded",
				Comment:          "Detent refused to dispatch this issue because the projected dispatch would exceed the daily budget.",
				CurrentSpendUSD:  2.40,
				ProjectedCostUSD: 0.20,
				MaxUSD:           &maxUSD,
				RefusedAt:        now,
			},
		},
	})

	refusal, ok := state.BudgetRefusals[issue.ID]
	if !ok {
		t.Fatal("BudgetRefusals missing issue, want recorded refusal")
	}
	if refusal.Issue.ID != issue.ID || refusal.Code != "per_day_max_usd" || refusal.ProjectedCostUSD != 0.20 {
		t.Fatalf("BudgetRefusal = %#v, want recorded issue and spend", refusal)
	}
	if orch.dispatchable(issue, &state, now.Add(time.Minute)) {
		t.Fatal("dispatchable during budget cooldown = true, want false")
	}
	if len(commentConnector.comments) != 1 {
		t.Fatalf("comments len = %d, want 1", len(commentConnector.comments))
	}
	if commentConnector.comments[0].issueID != issue.ID || !strings.Contains(commentConnector.comments[0].body, "projected dispatch would exceed the daily budget") {
		t.Fatalf("comment = %#v, want budget refusal comment", commentConnector.comments[0])
	}
}

func TestDispatchableFiltersUnauthorizedCandidates(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Authorization: selector.Selector{
			AssigneeIn: []string{"@me"},
		},
		SelectorContext: selector.Context{
			InstanceLogin: "worker-1",
			Persona:       "release-captain",
		},
	})
	orch := Orchestrator{cfg: cfg}

	tests := []struct {
		name  string
		issue connector.Issue
		want  bool
	}{
		{
			name: "matching assignee is dispatchable",
			issue: func() connector.Issue {
				issue := dispatchTestIssue("issue-authorized", "Todo")
				issue.Assignees = []string{"worker-1"}
				return issue
			}(),
			want: true,
		},
		{
			name: "nonmatching assignee is skipped",
			issue: func() connector.Issue {
				issue := dispatchTestIssue("issue-unauthorized", "Todo")
				issue.Assignees = []string{"worker-2"}
				return issue
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newState(cfg)
			if got := orch.dispatchable(tt.issue, &state, now); got != tt.want {
				t.Fatalf("dispatchable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthorizationFilterHintUsesTopLevelSelectorFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth selector.Selector
		ctx  selector.Context
		want connector.IssueFilterHint
	}{
		{
			name: "resolves identities and labels",
			auth: selector.Selector{
				AuthorIn:   []string{" alice ", "@me", "ALICE"},
				AssigneeIn: []string{" team-a ", "@me"},
				Labels: selector.Labels{
					Include: []string{" ready ", "READY"},
					Exclude: []string{" blocked "},
				},
			},
			ctx: selector.Context{
				InstanceLogin: "worker-1",
				Persona:       "release-captain",
			},
			want: connector.IssueFilterHint{
				Authors:      []string{"alice", "worker-1", "release-captain"},
				Assignees:    []string{"team-a", "worker-1", "release-captain"},
				LabelInclude: []string{"ready"},
				LabelExclude: []string{"blocked"},
			},
		},
		{
			name: "ignores nested selectors",
			auth: selector.Selector{
				And: []selector.Selector{{
					AuthorIn: []string{"nested-author"},
					Labels:   selector.Labels{Include: []string{"nested-label"}},
				}},
				Or: []selector.Selector{{
					AssigneeIn: []string{"nested-assignee"},
					Labels:     selector.Labels{Exclude: []string{"nested-exclude"}},
				}},
			},
			want: connector.IssueFilterHint{},
		},
		{
			name: "drops unresolved me token",
			auth: selector.Selector{
				AuthorIn:   []string{"@me"},
				AssigneeIn: []string{"@me"},
			},
			want: connector.IssueFilterHint{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := authorizationFilterHint(tt.auth, tt.ctx)
			assertIssueFilterHint(t, got, tt.want)
		})
	}
}

func TestFetchCandidateIssuesForTickPassesAuthorizationFilterHint(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		ActiveStates: []string{"Todo", "In Progress"},
		Authorization: selector.Selector{
			AuthorIn:   []string{"@me", "alice"},
			AssigneeIn: []string{"worker-2"},
			Labels: selector.Labels{
				Include: []string{"ready"},
				Exclude: []string{"blocked"},
			},
		},
		SelectorContext: selector.Context{
			InstanceLogin: "worker-1",
			Persona:       "release-captain",
		},
	})
	tracker := &filterFetchConnector{
		issues: []connector.Issue{dispatchTestIssue("issue-authorized", "Todo")},
	}
	orch := Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	got, err := orch.fetchCandidateIssuesForTick(context.Background(), &state)
	if err != nil {
		t.Fatalf("fetchCandidateIssuesForTick() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "issue-authorized" {
		t.Fatalf("fetchCandidateIssuesForTick() = %#v, want authorized issue", got)
	}
	if tracker.baseFetches != 0 {
		t.Fatalf("FetchCandidateIssues calls = %d, want 0", tracker.baseFetches)
	}
	if !slices.Equal(tracker.states, []string{"todo", "in progress"}) {
		t.Fatalf("states = %#v, want todo/in progress", tracker.states)
	}
	assertIssueFilterHint(t, tracker.hint, connector.IssueFilterHint{
		Authors:      []string{"worker-1", "release-captain", "alice"},
		Assignees:    []string{"worker-2"},
		LabelInclude: []string{"ready"},
		LabelExclude: []string{"blocked"},
	})
}

func TestDispatchPlanMatchesWithAndWithoutAuthorizationPushdown(t *testing.T) {
	t.Parallel()

	authorMatch := dispatchTestIssue("issue-author-match", "Todo")
	authorMatch.AuthorID = "alice"
	authorMiss := dispatchTestIssue("issue-author-miss", "Todo")
	authorMiss.AuthorID = "bob"

	combinedMatch := dispatchTestIssue("issue-combined-match", "Todo")
	combinedMatch.AuthorID = "worker-1"
	combinedMatch.Assignees = []string{"release-captain"}
	combinedMatch.Labels = []string{"ready", "team-a"}
	combinedMiss := dispatchTestIssue("issue-combined-miss", "Todo")
	combinedMiss.AuthorID = "worker-1"
	combinedMiss.Assignees = []string{"release-captain"}
	combinedMiss.Labels = []string{"blocked", "ready"}

	tests := []struct {
		name       string
		auth       selector.Selector
		ctx        selector.Context
		all        []connector.Issue
		pushedDown []connector.Issue
		want       []string
	}{
		{
			name:       "author hint",
			auth:       selector.Selector{AuthorIn: []string{"alice"}},
			all:        []connector.Issue{authorMatch, authorMiss},
			pushedDown: []connector.Issue{authorMatch},
			want:       []string{authorMatch.ID},
		},
		{
			name: "combined top-level hint",
			auth: selector.Selector{
				AuthorIn:   []string{"@me"},
				AssigneeIn: []string{"release-captain"},
				Labels: selector.Labels{
					Include: []string{"ready"},
					Exclude: []string{"blocked"},
				},
			},
			ctx: selector.Context{
				InstanceLogin: "worker-1",
				Persona:       "release-captain",
			},
			all:        []connector.Issue{combinedMatch, combinedMiss},
			pushedDown: []connector.Issue{combinedMatch},
			want:       []string{combinedMatch.ID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{
				MaxConcurrentAgents: 10,
				ActiveStates:        []string{"Todo"},
				TerminalStates:      []string{"Done"},
				Authorization:       tt.auth,
				SelectorContext:     tt.ctx,
			}
			withoutPushdown := dispatchPlanIssueIDs(cfg, tt.all)
			withPushdown := dispatchPlanIssueIDs(cfg, tt.pushedDown)
			if !slices.Equal(withoutPushdown, withPushdown) {
				t.Fatalf("dispatch IDs without pushdown = %#v, with pushdown = %#v", withoutPushdown, withPushdown)
			}
			if !slices.Equal(withPushdown, tt.want) {
				t.Fatalf("dispatch IDs = %#v, want %#v", withPushdown, tt.want)
			}
		})
	}
}

func TestMemoryConnectorOrchestratorsPartitionSharedIssuesByAuthorization(t *testing.T) {
	t.Parallel()

	alpha := dispatchTestIssue("issue-alpha", "Todo")
	alpha.Fields = map[string]string{"Owner": "alpha"}
	beta := dispatchTestIssue("issue-beta", "Todo")
	beta.Fields = map[string]string{"Owner": "beta"}
	sharedIssues := []connector.Issue{alpha, beta}

	tests := []struct {
		name      string
		owner     string
		wantIssue string
	}{
		{name: "alpha instance", owner: "alpha", wantIssue: alpha.ID},
		{name: "beta instance", owner: "beta", wantIssue: beta.ID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := newWorkerHostRunner()
			orch, err := New(Config{
				PollInterval:        time.Hour,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo"},
				TerminalStates:      []string{"Done"},
				Authorization: selector.Selector{
					Fields: []selector.FieldEquals{
						{Name: "Owner", Value: tt.owner},
					},
				},
			}, Dependencies{
				Connector: memory.New(memory.Config{Issues: sharedIssues}),
				Runner:    runner,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- orch.Run(ctx)
			}()

			request := receiveWorkerHostRunRequest(t, runner.started)
			if request.Issue.ID != tt.wantIssue {
				t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, tt.wantIssue)
			}

			select {
			case request := <-runner.started:
				t.Fatalf("unexpected extra dispatch = %#v", request)
			default:
			}

			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() error = %v, want context canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for orchestrator shutdown")
			}
		})
	}
}

func TestDispatchableChecksSlots(t *testing.T) {
	t.Parallel()

	now := time.Now()
	issue := dispatchTestIssue("issue-candidate", "Todo")

	tests := []struct {
		name  string
		cfg   Config
		state func(State)
		want  bool
	}{
		{
			name: "global cap full",
			cfg: Config{
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo"},
				TerminalStates:      []string{"Done"},
			},
			state: func(state State) {
				running := dispatchTestIssue("issue-running", "In Progress")
				state.Running[running.ID] = Running{Issue: running}
			},
			want: false,
		},
		{
			name: "per-state cap full",
			cfg: Config{
				MaxConcurrentAgents:        2,
				MaxConcurrentAgentsByState: map[string]int{"Todo": 1},
				ActiveStates:               []string{"Todo", "In Progress"},
				TerminalStates:             []string{"Done"},
			},
			state: func(state State) {
				running := dispatchTestIssue("issue-running", "Todo")
				state.Running[running.ID] = Running{Issue: running}
			},
			want: false,
		},
		{
			name: "per-state falls back to global cap",
			cfg: Config{
				MaxConcurrentAgents: 2,
				ActiveStates:        []string{"Todo", "In Progress"},
				TerminalStates:      []string{"Done"},
			},
			state: func(state State) {
				running := dispatchTestIssue("issue-running", "In Progress")
				state.Running[running.ID] = Running{Issue: running}
			},
			want: true,
		},
		{
			name: "per-host cap full",
			cfg: Config{
				MaxConcurrentAgents:        2,
				ActiveStates:               []string{"Todo"},
				TerminalStates:             []string{"Done"},
				WorkerHosts:                []string{"worker-a"},
				MaxConcurrentAgentsPerHost: 1,
			},
			state: func(state State) {
				running := dispatchTestIssue("issue-running", "Todo")
				state.Running[running.ID] = Running{Issue: running, WorkerHost: "worker-a"}
			},
			want: false,
		},
		{
			name: "alternate host has capacity",
			cfg: Config{
				MaxConcurrentAgents:        3,
				ActiveStates:               []string{"Todo"},
				TerminalStates:             []string{"Done"},
				WorkerHosts:                []string{"worker-a", "worker-b"},
				MaxConcurrentAgentsPerHost: 1,
			},
			state: func(state State) {
				running := dispatchTestIssue("issue-running", "Todo")
				state.Running[running.ID] = Running{Issue: running, WorkerHost: "worker-a"}
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(tt.cfg)
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			if tt.state != nil {
				tt.state(state)
			}

			got := orch.dispatchable(issue, &state, now)
			if got != tt.want {
				t.Fatalf("dispatchable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDispatchableSkipsDuplicatePullRequestWork(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 3,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done"},
	})
	orch := Orchestrator{cfg: cfg}

	tests := []struct {
		name  string
		issue connector.Issue
		want  bool
	}{
		{
			name:  "todo without pull request dispatches",
			issue: dispatchTestIssue("issue-no-pr", "Todo"),
			want:  true,
		},
		{
			name:  "todo with open pull request skips",
			issue: dispatchTestIssueWithPullRequest("issue-todo-open-pr", "Todo", "OPEN"),
			want:  false,
		},
		{
			name:  "todo with unavailable pull request hydration skips",
			issue: dispatchTestIssueWithUnavailablePullRequestHydration("issue-todo-pr-rate-limited", "Todo"),
			want:  false,
		},
		{
			name:  "todo with unknown unavailable pull request hydration dispatches",
			issue: dispatchTestIssueWithUnknownUnavailablePullRequestHydration("issue-todo-pr-unknown", "Todo"),
			want:  true,
		},
		{
			name:  "in progress with open pull request dispatches",
			issue: dispatchTestIssueWithPullRequest("issue-progress-open-pr", "In Progress", "OPEN"),
			want:  true,
		},
		{
			name:  "rework with open pull request dispatches",
			issue: dispatchTestIssueWithPullRequest("issue-rework-open-pr", "Rework", "OPEN"),
			want:  true,
		},
		{
			name:  "rework with unavailable pull request hydration skips",
			issue: dispatchTestIssueWithUnavailablePullRequestHydration("issue-rework-pr-rate-limited", "Rework"),
			want:  false,
		},
		{
			name:  "merging with open pull request dispatches",
			issue: dispatchTestIssueWithPullRequest("issue-merging-open-pr", "Merging", "OPEN"),
			want:  true,
		},
		{
			name: "merging with degraded pull request hydration skips",
			issue: func() connector.Issue {
				issue := dispatchTestIssueWithPullRequest("issue-merging-pr-degraded", "Merging", "OPEN")
				issue.PullRequest.HydrationDegradedReason = connector.PullRequestHydrationReasonStaleCachedPullData
				return issue
			}(),
			want: false,
		},
		{
			name:  "todo with merged pull request skips",
			issue: dispatchTestIssueWithPullRequest("issue-todo-merged-pr", "Todo", "MERGED"),
			want:  false,
		},
		{
			name:  "rework with merged pull request skips",
			issue: dispatchTestIssueWithPullRequest("issue-rework-merged-pr", "Rework", "MERGED"),
			want:  false,
		},
		{
			name: "rework with failed merged pull request dispatches",
			issue: func() connector.Issue {
				issue := dispatchTestIssueWithPullRequest("issue-rework-merged-pr-failed", "Rework", "MERGED")
				issue.PullRequest.CIStatus = "fail"
				issue.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{
					Name:       "Test",
					Status:     "completed",
					Conclusion: "failure",
				}}
				return issue
			}(),
			want: true,
		},
		{
			name:  "todo with closed unmerged pull request dispatches",
			issue: dispatchTestIssueWithPullRequest("issue-todo-closed-pr", "Todo", "CLOSED"),
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newState(cfg)
			got := orch.dispatchable(tt.issue, &state, now)
			if got != tt.want {
				t.Fatalf("dispatchable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDispatchPlanReportsMergedPullRequestReconciliationPending(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 15, 30, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 3,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done"},
	})
	issues := []connector.Issue{
		dispatchTestIssueWithPullRequest("issue-todo-merged-pr", "Todo", "MERGED"),
		func() connector.Issue {
			issue := dispatchTestIssueWithPullRequest("issue-todo-merged-pr-failed", "Todo", "MERGED")
			issue.PullRequest.CIStatus = "fail"
			issue.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{
				Name:       "Tier-1 Race Tests",
				Status:     "completed",
				Conclusion: "failure",
			}}
			return issue
		}(),
	}
	state := newState(cfg)
	decisions := make(map[string]dispatchPlanDecision)

	newDispatchPlanner(cfg).plan(&state, issues, now, dispatchPlanHooks{
		decision: func(decision dispatchPlanDecision) {
			decisions[decision.Issue.ID] = decision
		},
	})

	for _, issue := range issues {
		decision, ok := decisions[issue.ID]
		if !ok {
			t.Fatalf("decision for %s missing", issue.ID)
		}
		if decision.Selected {
			t.Fatalf("decision for %s selected = true, want reconciliation skip", issue.ID)
		}
		if decision.SkipReason != dispatchSkipMergedPullRequest {
			t.Fatalf("decision for %s skip reason = %q, want %q", issue.ID, decision.SkipReason, dispatchSkipMergedPullRequest)
		}
	}
}

func TestDispatchModeMergingFastPathFlag(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done"},
	})
	state := newState(cfg)
	issue := dispatchTestIssueWithPullRequest("issue-merging", "Merging", "OPEN")

	off := Orchestrator{cfg: cfg}
	if got := off.dispatchMode(context.Background(), &state, issue); got != runpkg.RunModeImplement {
		t.Fatalf("flag off dispatchMode = %q, want implement", got)
	}

	cfg.MergeFastPathEnabled = true
	on := Orchestrator{cfg: cfg}
	if got := on.dispatchMode(context.Background(), &state, issue); got != runpkg.RunModeMerge {
		t.Fatalf("flag on dispatchMode = %q, want merge", got)
	}
}

func TestDispatchCandidatesClaimsDuplicateIssueWithinCycle(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
	})
	runner := newWorkerHostRunner()
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Now()
	candidate := dispatchTestIssue("issue-duplicate", "Todo")

	ctx := t.Context()

	orch.dispatchCandidates(ctx, &state, []connector.Issue{candidate, candidate}, now)
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != candidate.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, candidate.ID)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected duplicate dispatch = %#v", request)
	default:
	}
	if len(state.Running) != 1 {
		t.Fatalf("Running len = %d, want 1", len(state.Running))
	}
	if len(state.Claimed) != 1 {
		t.Fatalf("Claimed len = %d, want 1", len(state.Claimed))
	}
}

func TestDispatchReadyIssuesPassesPriorAttemptToRunner(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Rework"},
		TerminalStates:      []string{"Done"},
	})
	runner := newWorkerHostRunner()
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Date(2026, 7, 2, 22, 0, 0, 0, time.UTC)
	issue := dispatchTestIssue("issue-prior-attempt", "Rework")
	state.PriorAttempts[issue.ID] = runpkg.PriorAttempt{
		Source: "auto_promote",
		Reason: "validator_rework",
		Validator: gate.ValidatorResult{
			Submitted: true,
			Verdict:   gate.ValidatorVerdictRework,
			Findings: []gate.Finding{{
				Severity: "p1",
				Body:     "Missing handoff.",
				Path:     "internal/runner/prompt.go",
				Line:     44,
			}},
		},
	}

	orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.PriorAttempt.Reason != "validator_rework" {
		t.Fatalf("PriorAttempt = %#v, want validator_rework", request.PriorAttempt)
	}
	if len(request.PriorAttempt.Validator.Findings) != 1 || request.PriorAttempt.Validator.Findings[0].Line != 44 {
		t.Fatalf("PriorAttempt.Validator.Findings = %#v", request.PriorAttempt.Validator.Findings)
	}
}

func TestDispatchReadyIssuesUsesLatestLaneTransitionContext(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Research", "Draft", "Review", "Package"},
		TerminalStates:      []string{"Publish"},
	})
	runner := newWorkerHostRunner()
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch := Orchestrator{
		cfg:             cfg,
		supervisor:      newTestSupervisor(t, runner, cfg),
		runResults:      make(chan runpkg.Completion),
		workflowMetrics: metrics,
	}
	state := newState(cfg)
	now := time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)
	issue := dispatchTestIssue("issue-package", "Package")
	if _, err := metrics.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{
		IssueID:           issue.ID,
		Identifier:        issue.Identifier,
		PhaseType:         store.WorkflowPhaseTypeLane,
		PhaseName:         "Package",
		PreviousPhaseName: "Review",
		Status:            "entered",
		StartedAt:         now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}

	orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.DispatchSourceState != "Review" || request.DispatchTargetState != "Package" {
		t.Fatalf("RunRequest dispatch transition = %q -> %q, want Review -> Package", request.DispatchSourceState, request.DispatchTargetState)
	}
}

func TestDispatchReadyIssuesRechecksStartTransitionStateCapacity(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"In Progress": 1,
		},
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	runner := newWorkerHostRunner()
	orch := Orchestrator{
		cfg:        cfg,
		connector:  hydratingDispatchConnector{},
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Now()
	first := dispatchTestIssue("issue-first", "Todo")
	first.Fields = map[string]string{"Status": "Todo"}
	second := dispatchTestIssue("issue-second", "Todo")
	second.Fields = map[string]string{"Status": "Todo"}

	orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{first, second}, now)

	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != first.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, first.ID)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected second dispatch = %#v", request)
	default:
	}
	if len(state.Running) != 1 {
		t.Fatalf("Running len = %d, want 1", len(state.Running))
	}
	if got := state.Running[first.ID].Issue.State; got != "In Progress" {
		t.Fatalf("Running[%q].Issue.State = %q, want In Progress", first.ID, got)
	}
}

func TestDispatchIssueRequiresSharedGlobalSlot(t *testing.T) {
	t.Parallel()

	global := scheduler.NewRoundRobin(scheduler.Config{Capacity: 1})
	globalGate := scheduler.NewGlobalDispatchGate(global)
	now := time.Now()
	ctx := t.Context()

	alphaRunner := newWorkerHostRunner()
	alphaCfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Project:             scheduler.ProjectCandidate{ID: "alpha", Weight: 1},
	})
	alpha := Orchestrator{
		cfg:                alphaCfg,
		supervisor:         newTestSupervisor(t, alphaRunner, alphaCfg),
		runResults:         make(chan runpkg.Completion),
		globalDispatchGate: globalGate,
	}
	alphaState := newState(alphaCfg)
	alphaIssue := dispatchTestIssue("issue-alpha", "Todo")

	if !alpha.dispatchIssue(ctx, &alphaState, alphaIssue, 0, now, "") {
		t.Fatal("alpha dispatchIssue() = false, want true")
	}
	alphaRequest := receiveWorkerHostRunRequest(t, alphaRunner.started)
	if alphaRequest.Issue.ID != alphaIssue.ID {
		t.Fatalf("alpha RunRequest.Issue.ID = %q, want %q", alphaRequest.Issue.ID, alphaIssue.ID)
	}

	bravoRunner := newWorkerHostRunner()
	bravoCfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Project:             scheduler.ProjectCandidate{ID: "bravo", Weight: 1},
	})
	bravo := Orchestrator{
		cfg:                bravoCfg,
		supervisor:         newTestSupervisor(t, bravoRunner, bravoCfg),
		runResults:         make(chan runpkg.Completion),
		globalDispatchGate: globalGate,
	}
	bravoState := newState(bravoCfg)
	bravoIssue := dispatchTestIssue("issue-bravo", "Todo")

	if bravo.dispatchIssue(ctx, &bravoState, bravoIssue, 0, now, "") {
		t.Fatal("bravo dispatchIssue() = true while global slot is held, want false")
	}
	select {
	case request := <-bravoRunner.started:
		t.Fatalf("unexpected bravo dispatch while global slot is held = %#v", request)
	default:
	}

	alpha.handleRunResult(ctx, &alphaState, runpkg.Completion{
		IssueID:     alphaIssue.ID,
		CompletedAt: now.Add(time.Second),
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	if !bravo.dispatchIssue(ctx, &bravoState, bravoIssue, 0, now.Add(2*time.Second), "") {
		t.Fatal("bravo dispatchIssue() after alpha completion = false, want true")
	}
	bravoRequest := receiveWorkerHostRunRequest(t, bravoRunner.started)
	if bravoRequest.Issue.ID != bravoIssue.ID {
		t.Fatalf("bravo RunRequest.Issue.ID = %q, want %q", bravoRequest.Issue.ID, bravoIssue.ID)
	}
}

func TestDispatchReadyIssuesAllowsOneMergeWorkerPerProject(t *testing.T) {
	t.Parallel()

	globalGate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 2}))
	now := time.Date(2026, 6, 25, 13, 0, 0, 0, time.UTC)
	ctx := t.Context()

	alphaRunner := newWorkerHostRunner()
	alphaCfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		DispatchPriorityByState: []string{"Merging", "Rework", "Todo"},
		ActiveStates:            []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:          []string{"Done"},
		Project:                 scheduler.ProjectCandidate{ID: "alpha", Weight: 1},
	})
	alpha := Orchestrator{
		cfg:                alphaCfg,
		supervisor:         newTestSupervisor(t, alphaRunner, alphaCfg),
		runResults:         make(chan runpkg.Completion),
		globalDispatchGate: globalGate,
	}
	alphaState := newState(alphaCfg)
	alphaFirst := dispatchTestIssueWithPullRequest("issue-alpha-first", "Merging", "OPEN")
	alphaSecond := dispatchTestIssueWithPullRequest("issue-alpha-second", "Merging", "OPEN")

	alpha.dispatchReadyIssues(ctx, &alphaState, []connector.Issue{alphaFirst, alphaSecond}, now)
	alphaRequest := receiveWorkerHostRunRequest(t, alphaRunner.started)
	if alphaRequest.Issue.ID != alphaFirst.ID {
		t.Fatalf("alpha RunRequest.Issue.ID = %q, want %q", alphaRequest.Issue.ID, alphaFirst.ID)
	}
	if len(alphaState.Running) != 1 {
		t.Fatalf("alpha Running len = %d, want 1", len(alphaState.Running))
	}

	bravoRunner := newWorkerHostRunner()
	bravoCfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		DispatchPriorityByState: []string{"Merging", "Rework", "Todo"},
		ActiveStates:            []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:          []string{"Done"},
		Project:                 scheduler.ProjectCandidate{ID: "bravo", Weight: 1},
	})
	bravo := Orchestrator{
		cfg:                bravoCfg,
		supervisor:         newTestSupervisor(t, bravoRunner, bravoCfg),
		runResults:         make(chan runpkg.Completion),
		globalDispatchGate: globalGate,
	}
	bravoState := newState(bravoCfg)
	bravoIssue := dispatchTestIssueWithPullRequest("issue-bravo", "Merging", "OPEN")

	bravo.dispatchReadyIssues(ctx, &bravoState, []connector.Issue{bravoIssue}, now.Add(time.Second))
	bravoRequest := receiveWorkerHostRunRequest(t, bravoRunner.started)
	if bravoRequest.Issue.ID != bravoIssue.ID {
		t.Fatalf("bravo RunRequest.Issue.ID = %q, want %q", bravoRequest.Issue.ID, bravoIssue.ID)
	}
	if len(bravoState.Running) != 1 {
		t.Fatalf("bravo Running len = %d, want 1", len(bravoState.Running))
	}

	for _, running := range alphaState.Running {
		if running.cancel != nil {
			running.cancel()
		}
	}
	for _, running := range bravoState.Running {
		if running.cancel != nil {
			running.cancel()
		}
	}
}

func TestDispatchReadyIssuesLogsMergeSlotDecisionAndStopsAfterGlobalWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)
	ctx := t.Context()
	globalGate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	lowerPriorityProject := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
	lowerSlot, ok, decision, err := globalGate.TryAcquireWithDecision(ctx, lowerPriorityProject, scheduler.SlotRequest{
		State:    "Todo",
		Priority: 2,
	}, now)
	if err != nil {
		t.Fatalf("lower-priority TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("lower-priority TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	t.Cleanup(func() {
		if err := globalGate.Release(lowerSlot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	})

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		DispatchPriorityByState: []string{"Merging", "Rework", "Todo"},
		ActiveStates:            []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:          []string{"Done"},
		Project:                 scheduler.ProjectCandidate{ID: "zulu", Weight: 1},
	})
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := Orchestrator{
		cfg:                cfg,
		supervisor:         newTestSupervisor(t, runner, cfg),
		runResults:         make(chan runpkg.Completion),
		globalDispatchGate: globalGate,
		logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	first := dispatchTestIssueWithPullRequest("issue-merge-first", "Merging", "OPEN")
	second := dispatchTestIssueWithPullRequest("issue-merge-second", "Merging", "OPEN")

	orch.dispatchReadyIssues(ctx, &state, []connector.Issue{first, second}, now.Add(time.Second))

	select {
	case request := <-runner.started:
		t.Fatalf("unexpected merge dispatch while global slot is held = %#v", request)
	default:
	}
	if len(state.Running) != 0 {
		t.Fatalf("Running len = %d, want 0", len(state.Running))
	}
	logText := logs.String()
	if count := strings.Count(logText, "merge_worker_slot_wait"); count != 1 {
		t.Fatalf("merge_worker_slot_wait count = %d, want 1; logs = %q", count, logText)
	}
	for _, fragment := range []string{
		"reason=global_capacity_full",
		"global_capacity=1",
		"global_used=1",
		"global_available=0",
		"project_state_capacity=1",
		"project_state_used=0",
		"project_state_available=1",
		"lower_priority_running=1",
		"selected_project_id=zulu",
		"selected_state=merging",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs %q missing fragment %q", logText, fragment)
		}
	}
	if strings.Contains(logText, "merge_worker_failure") {
		t.Fatalf("logs %q contain merge_worker_failure, want merge slot wait telemetry instead", logText)
	}
}

func TestDispatchReadyIssuesRecordsNonMergeSlotWaitTelemetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	ctx := t.Context()
	globalGate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	lowerSlot, ok, decision, err := globalGate.TryAcquireWithDecision(ctx, scheduler.ProjectCandidate{ID: "alpha", Weight: 1}, scheduler.SlotRequest{
		State:    "Todo",
		Priority: 2,
	}, now)
	if err != nil {
		t.Fatalf("lower-priority TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("lower-priority TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	t.Cleanup(func() {
		if err := globalGate.Release(lowerSlot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	})

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:     2,
		DispatchPriorityByState: []string{"Merging", "Rework", "Todo"},
		ActiveStates:            []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:          []string{"Done"},
		Project:                 scheduler.ProjectCandidate{ID: "zulu", Weight: 1},
	})
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := Orchestrator{
		cfg:                cfg,
		supervisor:         newTestSupervisor(t, runner, cfg),
		runResults:         make(chan runpkg.Completion),
		globalDispatchGate: globalGate,
		logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	candidate := dispatchTestIssue("issue-rework", "Rework")

	orch.dispatchReadyIssues(ctx, &state, []connector.Issue{candidate}, now.Add(time.Second))

	select {
	case request := <-runner.started:
		t.Fatalf("unexpected rework dispatch while global slot is held = %#v", request)
	default:
	}
	if len(state.Running) != 0 {
		t.Fatalf("Running len = %d, want 0", len(state.Running))
	}
	logText := logs.String()
	for _, fragment := range []string{
		"dispatch_slot_wait",
		"issue_id=issue-rework",
		"state=Rework",
		"reason=global_capacity_full",
		"global_capacity=1",
		"global_used=1",
		"global_available=0",
		"project_state_capacity=2",
		"project_state_used=0",
		"project_state_available=2",
		"selected_project_id=zulu",
		"selected_state=rework",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs %q missing fragment %q", logText, fragment)
		}
	}
	if strings.Contains(logText, "merge_worker_failure") {
		t.Fatalf("logs %q contain merge_worker_failure, want dispatch slot wait telemetry instead", logText)
	}
	if len(state.RecentEvents) != 1 {
		t.Fatalf("RecentEvents len = %d, want 1", len(state.RecentEvents))
	}
	event := state.RecentEvents[0]
	if event.Event != "dispatch_slot_wait" {
		t.Fatalf("RecentEvents[0].Event = %q, want dispatch_slot_wait", event.Event)
	}
	for _, fragment := range []string{
		"digitaldrywood/detent#issue-rework",
		"state=Rework",
		"reason=global_capacity_full",
		"global_available=0",
		"project_state_available=2",
		"selected_project_id=zulu",
	} {
		if !strings.Contains(event.Message, fragment) {
			t.Fatalf("RecentEvents[0].Message %q missing fragment %q", event.Message, fragment)
		}
	}
}

func TestDispatchReadyIssuesHydratesLightweightCandidateBeforeDependencyGate(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
	})
	runner := newWorkerHostRunner()
	candidate := dispatchTestIssue("issue-lightweight", "Todo")
	candidate.Fields = map[string]string{}
	candidate.BlockedBy = nil
	hydrated := candidate
	hydrated.Fields = map[string]string{}
	hydrated.BlockedBy = []connector.BlockedRef{{
		Identifier: "digitaldrywood/detent#issue-blocker",
		State:      "In Progress",
	}}
	orch := Orchestrator{
		cfg:        cfg,
		connector:  hydratingDispatchConnector{issue: hydrated},
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Now()

	ctx := t.Context()

	orch.dispatchReadyIssues(ctx, &state, []connector.Issue{candidate}, now)
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected dispatch for hydrated blocked candidate = %#v", request)
	default:
	}
	blocked, ok := state.Blocked[candidate.ID]
	if !ok {
		t.Fatalf("Blocked[%q] missing after hydrated dependency gate", candidate.ID)
	}
	if blocked.Issue.BlockedBy[0].State != "In Progress" {
		t.Fatalf("blocked dependency state = %q, want In Progress", blocked.Issue.BlockedBy[0].State)
	}
}

func TestDispatchReadyIssuesLogsDebugDecisionAndWorkerLifecycle(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		Project:             scheduler.ProjectCandidate{ID: "detent", Weight: 2, Priority: 10},
	})
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := Orchestrator{
		cfg:        cfg,
		connector:  hydratingDispatchConnector{},
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
	state := newState(cfg)
	running := dispatchTestIssueWithPullRequest("issue-running", "In Progress", "OPEN")
	running.Fields = map[string]string{"Status": "In Progress"}
	running.PullRequest.CIStatus = "pass"
	running.PullRequest.CheckRunCount = 3
	state.Running[running.ID] = Running{Issue: running, StartedAt: now.Add(-time.Minute)}
	selected := dispatchTestIssue("issue-selected", "Todo")
	selected.Fields = map[string]string{"Status": "Todo"}
	selected.UpdatedAt = timePointer(now.Add(-2 * time.Minute))

	ctx := t.Context()
	orch.dispatchReadyIssues(ctx, &state, []connector.Issue{running, selected}, now)

	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != selected.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, selected.ID)
	}
	if state.Running[selected.ID].cancel == nil {
		t.Fatalf("Running[%q].cancel = nil, want cancellation hook", selected.ID)
	}
	state.Running[selected.ID].cancel()
	orch.handleRunResult(ctx, &state, runpkg.Completion{
		IssueID:     selected.ID,
		Request:     request,
		Err:         context.Canceled,
		CompletedAt: now.Add(time.Second),
	})

	logText := logs.String()
	for _, fragment := range []string{
		"scheduler_dispatch_decision",
		"skip_reason=already_running",
		"result=selected",
		"queue_position=2",
		"pr_ci_status=pass",
		"pr_check_run_count=3",
		"scheduler_dispatch_slot_decision",
		"outcome=acquired",
		"worker_slot_acquired",
		"worker_attempt_started",
		"worker_capacity_released",
		"worker_cancelled",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs missing %q:\n%s", fragment, logText)
		}
	}
}

func TestDispatchIssueAcquiresDurableAttemptBeforeWorker(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 14, 45, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		WorkerHosts:         []string{"worker-a"},
	})
	runner := newWorkerHostRunner()
	attempts := &recordingWorkAttemptStore{nextID: 42}
	orch := Orchestrator{
		cfg:          cfg,
		supervisor:   newTestSupervisor(t, runner, cfg),
		runResults:   make(chan runpkg.Completion, 1),
		workAttempts: attempts,
	}
	state := newState(cfg)
	issue := dispatchTestIssue("issue-durable-attempt", "Todo")
	issue.Identifier = "digitaldrywood/detent#737"
	issue.URL = "https://github.com/digitaldrywood/detent/issues/737"

	if !orch.dispatchIssue(t.Context(), &state, issue, 2, now, "worker-a") {
		t.Fatal("dispatchIssue() = false, want true")
	}
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.WorkAttemptID != 42 {
		t.Fatalf("RunRequest.WorkAttemptID = %d, want 42", request.WorkAttemptID)
	}
	running := state.Running[issue.ID]
	if running.WorkAttemptID != 42 {
		t.Fatalf("Running.WorkAttemptID = %d, want 42", running.WorkAttemptID)
	}
	if len(attempts.starts) != 1 {
		t.Fatalf("work attempt starts len = %d, want 1", len(attempts.starts))
	}
	start := attempts.starts[0]
	if start.ProjectID != "detent" || start.IssueID != issue.ID || start.Identifier != issue.Identifier {
		t.Fatalf("work attempt start identity = %#v, want detent issue", start)
	}
	if start.WorkerType != "agent" || start.WorkerHost != "worker-a" || start.AttemptNumber != 2 {
		t.Fatalf("work attempt start worker = %#v, want agent worker-a attempt 2", start)
	}
	if !start.StartedAt.Equal(now) || !start.LeaseExpiresAt.After(now) {
		t.Fatalf("work attempt times = started %s lease %s, want started now and future lease", start.StartedAt, start.LeaseExpiresAt)
	}
	state.Running[issue.ID].cancel()
}

func TestHandleRunResultCompletesDurableAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Project:             scheduler.ProjectCandidate{ID: "detent"},
	})
	attempts := &recordingWorkAttemptStore{}
	retrospector := &recordingRetrospector{}
	orch := Orchestrator{
		cfg:          cfg,
		runResults:   make(chan runpkg.Completion, 1),
		workAttempts: attempts,
		retrospector: retrospector,
	}
	state := newState(cfg)
	issue := dispatchTestIssue("issue-failed-attempt", "Todo")
	state.Running[issue.ID] = Running{
		Issue:         issue,
		Attempt:       2,
		StartedAt:     now.Add(-time.Minute),
		WorkerHost:    "worker-a",
		WorkAttemptID: 77,
	}
	errRun := errors.New("runner eof")

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Request:      RunRequest{Issue: issue, Attempt: 2, WorkAttemptID: 77},
		Err:          errRun,
		CompletedAt:  now,
		Retryable:    true,
		RetryAttempt: 3,
		RetryDelay:   time.Second,
	})

	if len(attempts.completions) != 1 {
		t.Fatalf("work attempt completions len = %d, want 1", len(attempts.completions))
	}
	completion := attempts.completions[0]
	if completion.AttemptID != 77 || completion.TerminalState != store.WorkAttemptTerminalFailure {
		t.Fatalf("completion = %#v, want failed attempt 77", completion)
	}
	if completion.ErrorClass != "runner_error" || !strings.Contains(completion.ErrorMessage, "runner eof") {
		t.Fatalf("completion error = %q/%q, want runner_error with message", completion.ErrorClass, completion.ErrorMessage)
	}
	if _, ok := state.Retry[issue.ID]; !ok {
		t.Fatalf("Retry[%q] missing after failed durable attempt", issue.ID)
	}
	if !slices.Equal(retrospector.triggers, []string{"completion"}) {
		t.Fatalf("retrospector triggers = %v, want completion", retrospector.triggers)
	}
}

func TestDispatchReadyIssuesPersistsEveryCapacitySkip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 15, 15, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		Project:             scheduler.ProjectCandidate{ID: "detent"},
	})
	attempts := &recordingWorkAttemptStore{}
	orch := Orchestrator{
		cfg:          cfg,
		runResults:   make(chan runpkg.Completion, 1),
		workAttempts: attempts,
	}
	state := newState(cfg)
	running := dispatchTestIssue("issue-running-capacity", "Todo")
	state.Running[running.ID] = Running{Issue: running, StartedAt: now.Add(-time.Minute)}
	first := dispatchTestIssue("issue-waiting-a", "Todo")
	second := dispatchTestIssue("issue-waiting-b", "Todo")

	orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{first, second}, now)

	if len(attempts.decisions) != 2 {
		t.Fatalf("decisions len = %d, want every skipped candidate: %#v", len(attempts.decisions), attempts.decisions)
	}
	for _, decision := range attempts.decisions {
		if decision.Result != store.SchedulerDecisionResultSkipped || decision.Reason != dispatchSkipGlobalCapacityFull {
			t.Fatalf("decision = %#v, want skipped global capacity", decision)
		}
	}
}

func TestDispatchReadyIssuesStaggersContinuationDispatches(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
	})
	runner := newWorkerHostRunner()
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Now()
	first := dispatchTestIssueWithPullRequest("issue-first", "In Progress", "OPEN")
	second := dispatchTestIssueWithPullRequest("issue-second", "In Progress", "OPEN")

	ctx := t.Context()

	done := make(chan struct{})
	go func() {
		defer close(done)
		orch.dispatchReadyIssues(ctx, &state, []connector.Issue{first, second}, now)
	}()

	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != first.ID {
		t.Fatalf("first RunRequest.Issue.ID = %q, want %q", request.Issue.ID, first.ID)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected unstaggered continuation dispatch = %#v", request)
	default:
	}

	request = receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != second.ID {
		t.Fatalf("second RunRequest.Issue.ID = %q, want %q", request.Issue.ID, second.ID)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dispatchReadyIssues to finish")
	}
}

func TestContinuationDelayUsesConstantGap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		index int
		want  time.Duration
	}{
		{index: -1, want: 0},
		{index: 0, want: 0},
		{index: 1, want: continuationDispatchBackoff},
		{index: 2, want: continuationDispatchBackoff},
		{index: 50, want: continuationDispatchBackoff},
	}

	for _, tt := range tests {
		got := continuationDelay(tt.index)
		if got != tt.want {
			t.Fatalf("continuationDelay(%d) = %s, want %s", tt.index, got, tt.want)
		}
	}
}

func TestDispatchCandidatesAssignsLeastLoadedWorkerHost(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:        3,
		ActiveStates:               []string{"Todo"},
		TerminalStates:             []string{"Done"},
		WorkerHosts:                []string{"worker-a", "worker-b"},
		MaxConcurrentAgentsPerHost: 1,
	})
	runner := newWorkerHostRunner()
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Now()
	running := dispatchTestIssue("issue-running", "Todo")
	state.Running[running.ID] = Running{Issue: running, WorkerHost: "worker-a"}
	candidate := dispatchTestIssue("issue-candidate", "Todo")

	ctx := t.Context()

	orch.dispatchCandidates(ctx, &state, []connector.Issue{candidate}, now)
	request := receiveWorkerHostRunRequest(t, runner.started)

	if request.WorkerHost != "worker-b" {
		t.Fatalf("RunRequest.WorkerHost = %q, want worker-b", request.WorkerHost)
	}
	if got := state.Running[candidate.ID].WorkerHost; got != "worker-b" {
		t.Fatalf("Running[%q].WorkerHost = %q, want worker-b", candidate.ID, got)
	}
}

func TestDispatchIssueIncludesSelectorContext(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		SelectorPersona:     " persona-reviewer ",
	})
	runner := newWorkerHostRunner()
	orch := Orchestrator{
		cfg:        cfg,
		connector:  selectorContextConnector{login: "worker-1"},
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion),
	}
	state := newState(cfg)
	now := time.Now()
	issue := dispatchTestIssue("issue-selector-context", "Todo")

	ctx := t.Context()

	orch.dispatchIssue(ctx, &state, issue, 0, now, "")
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.SelectorContext.InstanceLogin != "worker-1" {
		t.Fatalf("SelectorContext.InstanceLogin = %q, want worker-1", request.SelectorContext.InstanceLogin)
	}
	if request.SelectorContext.Persona != "persona-reviewer" {
		t.Fatalf("SelectorContext.Persona = %q, want persona-reviewer", request.SelectorContext.Persona)
	}
}

func TestDispatchIssueClearsReapedWorkspaceMarker(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	now := time.Now()
	issue := dispatchTestIssue("issue-reopened", "Todo")
	state.ReapedWorkspaces[issue.ID] = now.Add(-time.Hour)

	if !orch.dispatchIssue(context.Background(), &state, issue, 0, now, "") {
		t.Fatal("dispatchIssue() = false, want true")
	}
	if _, ok := state.ReapedWorkspaces[issue.ID]; ok {
		t.Fatalf("ReapedWorkspaces[%q] present after dispatch", issue.ID)
	}
}

func TestSelectWorkerHostKeepsPreferredHostWhenAvailable(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:        3,
		ActiveStates:               []string{"Todo"},
		TerminalStates:             []string{"Done"},
		WorkerHosts:                []string{"worker-a", "worker-b"},
		MaxConcurrentAgentsPerHost: 2,
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	running := dispatchTestIssue("issue-running", "Todo")
	state.Running[running.ID] = Running{Issue: running, WorkerHost: "worker-a"}

	host, ok := orch.selectWorkerHost(&state, "worker-a")
	if !ok {
		t.Fatal("selectWorkerHost() ok = false, want true")
	}
	if host != "worker-a" {
		t.Fatalf("selectWorkerHost() host = %q, want worker-a", host)
	}
}

func dispatchTestIssue(id, state string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#" + id
	issue.Title = "Dispatch test issue"
	issue.State = state
	return issue
}

func dispatchTestIssueWithPullRequest(id, state, prState string) connector.Issue {
	issue := dispatchTestIssue(id, state)
	issue.PullRequest = &connector.PullRequest{
		Number:     187,
		URL:        "https://github.com/digitaldrywood/detent/pull/187",
		BranchName: "detent/digitaldrywood_detent_187",
		State:      prState,
	}
	return issue
}

func dispatchTestIssueWithUnavailablePullRequestHydration(id, state string) connector.Issue {
	issue := dispatchTestIssueWithPullRequest(id, state, "OPEN")
	issue.PullRequest.HydrationUnavailableReason = "rate_limited"
	return issue
}

func dispatchTestIssueWithUnknownUnavailablePullRequestHydration(id, state string) connector.Issue {
	issue := dispatchTestIssue(id, state)
	issue.PullRequest = &connector.PullRequest{
		HydrationUnavailableReason: "rest_budget_reserved",
	}
	return issue
}

func assertIssueFilterHint(t *testing.T, got connector.IssueFilterHint, want connector.IssueFilterHint) {
	t.Helper()

	if !slices.Equal(got.Authors, want.Authors) {
		t.Fatalf("Authors = %#v, want %#v", got.Authors, want.Authors)
	}
	if !slices.Equal(got.Assignees, want.Assignees) {
		t.Fatalf("Assignees = %#v, want %#v", got.Assignees, want.Assignees)
	}
	if !slices.Equal(got.LabelInclude, want.LabelInclude) {
		t.Fatalf("LabelInclude = %#v, want %#v", got.LabelInclude, want.LabelInclude)
	}
	if !slices.Equal(got.LabelExclude, want.LabelExclude) {
		t.Fatalf("LabelExclude = %#v, want %#v", got.LabelExclude, want.LabelExclude)
	}
}

func dispatchPlanIssueIDs(cfg Config, candidates []connector.Issue) []string {
	cfg = normalizeConfig(cfg)
	state := newState(cfg)
	plan := newDispatchPlanner(cfg).plan(&state, candidates, time.Now(), dispatchPlanHooks{})
	ids := make([]string, 0, len(plan.Dispatches))
	for _, dispatch := range plan.Dispatches {
		ids = append(ids, dispatch.IssueID)
	}
	return ids
}

type filterFetchConnector struct {
	issues      []connector.Issue
	states      []string
	hint        connector.IssueFilterHint
	baseFetches int
}

func (c *filterFetchConnector) Name() string {
	return "filter-fetch"
}

func (c *filterFetchConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.baseFetches++
	return cloneIssues(c.issues), nil
}

func (c *filterFetchConnector) FetchCandidateIssuesByStatesWithFilter(
	_ context.Context,
	states []string,
	hint connector.IssueFilterHint,
) ([]connector.Issue, error) {
	c.states = append([]string(nil), states...)
	c.hint = connector.IssueFilterHint{
		Authors:      append([]string(nil), hint.Authors...),
		Assignees:    append([]string(nil), hint.Assignees...),
		LabelInclude: append([]string(nil), hint.LabelInclude...),
		LabelExclude: append([]string(nil), hint.LabelExclude...),
	}
	return cloneIssues(c.issues), nil
}

func (c *filterFetchConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *filterFetchConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *filterFetchConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *filterFetchConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c *filterFetchConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *filterFetchConnector) SetField(context.Context, string, string, string) error {
	return nil
}

type budgetRefusalComment struct {
	issueID string
	body    string
}

type budgetRefusalCommentConnector struct {
	comments []budgetRefusalComment
}

func (c *budgetRefusalCommentConnector) Name() string {
	return "budget-refusal-comment"
}

func (c *budgetRefusalCommentConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *budgetRefusalCommentConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *budgetRefusalCommentConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *budgetRefusalCommentConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, budgetRefusalComment{issueID: issueID, body: body})
	return nil
}

func (c *budgetRefusalCommentConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c *budgetRefusalCommentConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *budgetRefusalCommentConnector) SetField(context.Context, string, string, string) error {
	return nil
}

var _ connector.Connector = (*budgetRefusalCommentConnector)(nil)

type hydratingDispatchConnector struct {
	issue connector.Issue
}

func (c hydratingDispatchConnector) Name() string {
	return "hydrating"
}

func (c hydratingDispatchConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{c.issue}, nil
}

func (c hydratingDispatchConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c hydratingDispatchConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	if slices.Contains(ids, c.issue.ID) {
		return []connector.Issue{c.issue}, nil
	}
	return nil, nil
}

func (c hydratingDispatchConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c hydratingDispatchConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c hydratingDispatchConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c hydratingDispatchConnector) SetField(context.Context, string, string, string) error {
	return nil
}

type workerHostRunner struct {
	started chan RunRequest
}

type selectorContextConnector struct {
	connector.Connector
	login string
}

func (c selectorContextConnector) InstanceLogin() string {
	return c.login
}

func newWorkerHostRunner() *workerHostRunner {
	return &workerHostRunner{started: make(chan RunRequest, 1)}
}

func (r *workerHostRunner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	select {
	case r.started <- request:
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}

	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

func receiveWorkerHostRunRequest(t *testing.T, requests <-chan RunRequest) RunRequest {
	t.Helper()

	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker host run request")
	}

	return RunRequest{}
}

type recordingWorkAttemptStore struct {
	nextID         int64
	starts         []store.WorkAttemptStart
	heartbeats     []store.WorkAttemptHeartbeat
	completions    []store.WorkAttemptCompletion
	decisions      []store.SchedulerDecision
	reclaimed      []store.WorkAttempt
	recent         []store.WorkAttempt
	history        []store.WorkAttempt
	historyQueries []store.WorkAttemptHistoryQuery
}

type recordingRetrospector struct {
	triggers []string
}

func (r *recordingRetrospector) Trigger(trigger string) {
	r.triggers = append(r.triggers, trigger)
}

func (s *recordingWorkAttemptStore) StartWorkAttempt(_ context.Context, attrs store.WorkAttemptStart) (int64, error) {
	s.starts = append(s.starts, attrs)
	if s.nextID <= 0 {
		s.nextID = 1
	}
	id := s.nextID
	s.nextID++
	return id, nil
}

func (s *recordingWorkAttemptStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return store.WorkAttempt{}, store.ErrNotFound
}

func (s *recordingWorkAttemptStore) RecordWorkAttemptHeartbeat(_ context.Context, attrs store.WorkAttemptHeartbeat) error {
	s.heartbeats = append(s.heartbeats, attrs)
	return nil
}

func (s *recordingWorkAttemptStore) CompleteWorkAttempt(_ context.Context, attrs store.WorkAttemptCompletion) error {
	s.completions = append(s.completions, attrs)
	return nil
}

func (s *recordingWorkAttemptStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return append([]store.WorkAttempt(nil), s.reclaimed...), nil
}

func (s *recordingWorkAttemptStore) ReclaimActiveWorkAttempts(context.Context, store.WorkAttemptReclaim) ([]store.WorkAttempt, error) {
	return append([]store.WorkAttempt(nil), s.reclaimed...), nil
}

func (s *recordingWorkAttemptStore) ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *recordingWorkAttemptStore) ListRecentTerminalWorkAttempts(_ context.Context, query store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	s.historyQueries = append(s.historyQueries, query)
	if s.history == nil {
		return append([]store.WorkAttempt(nil), s.recent...), nil
	}
	return append([]store.WorkAttempt(nil), s.history...), nil
}

func (s *recordingWorkAttemptStore) RecordSchedulerDecision(_ context.Context, attrs store.SchedulerDecision) (int64, error) {
	s.decisions = append(s.decisions, attrs)
	return int64(len(s.decisions)), nil
}

func (s *recordingWorkAttemptStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}
