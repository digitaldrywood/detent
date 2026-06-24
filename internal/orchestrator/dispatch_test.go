package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
)

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
			name:  "merging with open pull request dispatches",
			issue: dispatchTestIssueWithPullRequest("issue-merging-open-pr", "Merging", "OPEN"),
			want:  true,
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
