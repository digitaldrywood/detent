package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestDispatchParityWithElixirRecordedCandidateSets(t *testing.T) {
	t.Parallel()

	fixture := loadDispatchParityFixture(t)
	for _, tt := range fixture.Cases {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			now := mustParseParityTime(t, tt.Now)
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents:        tt.Config.MaxConcurrentAgents,
				MaxConcurrentAgentsByState: tt.Config.MaxConcurrentAgentsByState,
				DispatchPriorityByState:    tt.Config.DispatchPriorityByState,
				DispatchPriorityByLabel:    tt.Config.DispatchPriorityByLabel,
				ActiveStates:               tt.Config.ActiveStates,
				TerminalStates:             tt.Config.TerminalStates,
				WorkerHosts:                tt.Config.WorkerHosts,
				MaxConcurrentAgentsPerHost: tt.Config.MaxConcurrentAgentsPerHost,
				BudgetRefusalCooldown:      time.Duration(tt.Config.BudgetRefusalCooldownSeconds) * time.Second,
			})
			orch := Orchestrator{
				cfg:        cfg,
				supervisor: newTestSupervisor(t, parityBlockingRunner{}, cfg),
				runResults: make(chan runpkg.Completion),
			}
			state := newState(cfg)
			applyParityInitialState(t, &state, tt.InitialState, now)

			candidates := parityIssues(t, tt.Candidates)
			sortIssuesForDispatch(candidates, cfg.DispatchPriorityByState, cfg.DispatchPriorityByLabel, cfg.PrioritizeUnblockers)
			orch.pruneBudgetRefusals(context.Background(), &state, now)
			orch.trackBlockedCandidates(&state, candidates, now)

			gotOrder := parityDispatchOrder(&orch, state.clone(), candidates, now)
			if !slices.Equal(gotOrder, tt.Want.DispatchOrder) {
				t.Fatalf("dispatch order = %#v, want %#v", gotOrder, tt.Want.DispatchOrder)
			}

			plan := PlanDispatch(cfg, state.clone(), candidates, now)
			if !slices.Equal(plan.DispatchOrder(), tt.Want.DispatchOrder) {
				t.Fatalf("PlanDispatch order = %#v, want %#v", plan.DispatchOrder(), tt.Want.DispatchOrder)
			}
			assertParitySet(t, "PlanDispatch blocked", plan.Blocked, tt.Want.Blocked)
			assertParitySet(t, "PlanDispatch claimed", plan.Claimed, tt.Want.Claimed)
			assertParitySet(t, "PlanDispatch refusals", plan.BudgetRefusals, tt.Want.Refusals)

			ctx, cancel := context.WithCancel(context.Background())
			orch.dispatchReadyIssues(ctx, &state, candidates, now)
			cancel()

			assertParitySet(t, "blocked", stateIDs(state.Blocked), tt.Want.Blocked)
			assertParitySet(t, "claimed", stateIDs(state.Claimed), tt.Want.Claimed)
			assertParitySet(t, "refusals", stateIDs(state.BudgetRefusals), tt.Want.Refusals)
		})
	}
}

func TestDispatchParityAdversarialFixtures(t *testing.T) {
	requireSoak(t)

	fixture := loadDispatchParityFixture(t)
	if len(fixture.AdversarialCases) == 0 {
		t.Fatal("dispatch parity fixture contains no adversarial cases")
	}
	for _, tt := range fixture.AdversarialCases {
		t.Run(tt.Name, func(t *testing.T) {
			switch tt.Behavior {
			case "always_blocked_same_reason":
				runAlwaysBlockedParityCase(t, tt)
			case "rate_limit_storm":
				runRateLimitStormParityCase(t, tt)
			case "gate_wait_timeout_loop":
				runGateWaitTimeoutParityCase(t, tt)
			default:
				t.Fatalf("unknown adversarial behavior %q", tt.Behavior)
			}
		})
	}
}

func runAlwaysBlockedParityCase(t *testing.T, tt dispatchParityAdversarialCase) {
	t.Helper()
	now := mustParseParityTime(t, tt.Now)
	clock := &soakClock{now: now}
	issue := connector.Issue{
		ID:               "always-blocked",
		Identifier:       "example/adversarial#blocked",
		Title:            "Always blocked with the same human-only reason",
		State:            blockedStatusState,
		AssignedToWorker: true,
	}
	tracker := newSoakConnector(issue)
	attempts := newSoakAttemptStore()
	runner := &soakSuccessRunner{clock: clock, spend: attempts, tokensPerRun: 1}
	orch, err := New(Config{
		Project:        scheduler.ProjectCandidate{ID: "parity"},
		ActiveStates:   []string{"Todo", "In Progress", "Rework"},
		ObservedStates: []string{blockedStatusState},
		TerminalStates: []string{"Done", "Cancelled"},
	}, Dependencies{Connector: tracker, Runner: runner, Now: clock.Now})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)
	for range tt.SimulatedTicks {
		orch.tick(t.Context(), &state, clock.Advance(time.Second))
	}
	assertAdversarialParityOutcome(t, tt, tracker.issueState(issue.ID), runner.callCount())
	if blocked, ok := state.Blocked[issue.ID]; !ok || blocked.Issue.State != blockedStatusState {
		t.Fatalf("Blocked[%q] = %#v, want parked issue", issue.ID, blocked)
	}
}

func runRateLimitStormParityCase(t *testing.T, tt dispatchParityAdversarialCase) {
	t.Helper()
	now := mustParseParityTime(t, tt.Now)
	clock := &soakClock{now: now}
	issue := connector.Issue{
		ID:               "rate-limit-storm",
		Identifier:       "example/adversarial#rate-limit",
		Title:            "REST rate limit storm",
		State:            "In Progress",
		AssignedToWorker: true,
	}
	tracker := &parityRateLimitConnector{
		soakConnector: newSoakConnector(issue),
		resetAt:       now.Add(time.Hour),
	}
	attempts := newSoakAttemptStore()
	runner := &soakSuccessRunner{clock: clock, spend: attempts, tokensPerRun: 1}
	orch, err := New(Config{
		Project:              scheduler.ProjectCandidate{ID: "parity"},
		ActiveStates:         []string{"Todo", "In Progress", "Rework"},
		ObservedStates:       []string{blockedStatusState},
		TerminalStates:       []string{"Done", "Cancelled"},
		GitHubRESTMinReserve: 1_000,
	}, Dependencies{Connector: tracker, Runner: runner, Now: clock.Now})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)
	for range tt.SimulatedTicks {
		orch.tick(t.Context(), &state, clock.Advance(time.Second))
	}
	assertAdversarialParityOutcome(t, tt, tracker.issueState(issue.ID), runner.callCount())
	if _, ok := activeGitHubRESTCapacityOutage(&state, clock.Now()); !ok {
		t.Fatalf("BackendOutages = %#v, want active REST capacity outage", state.BackendOutages)
	}
}

func runGateWaitTimeoutParityCase(t *testing.T, tt dispatchParityAdversarialCase) {
	t.Helper()
	now := mustParseParityTime(t, tt.Now)
	issue := connector.Issue{
		ID:               "gate-wait-timeout",
		Identifier:       "example/adversarial#gate-wait",
		Title:            "Gate wait timeout loop",
		State:            "In Progress",
		AssignedToWorker: true,
		PullRequest: &connector.PullRequest{
			Number:         42,
			URL:            "https://github.test/example/adversarial/pull/42",
			State:          "OPEN",
			MergeableState: "unknown",
			CIStatus:       "pending",
		},
	}
	tracker := newSoakConnector(issue)
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:         true,
			GateWaitTimeout: 15 * time.Minute,
			Gate:            gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-16 * time.Minute),
		FinalState:  FinalStateCompleted,
	}
	for range tt.SimulatedTicks {
		issues, err := tracker.FetchCandidateIssues(t.Context())
		if err != nil {
			t.Fatalf("FetchCandidateIssues() error = %v", err)
		}
		orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, issues, now)
		now = now.Add(time.Second)
	}
	assertAdversarialParityOutcome(t, tt, tracker.issueState(issue.ID), 0)
	if comments := tracker.commentCount(issue.ID); comments != 1 {
		t.Fatalf("timeout comments = %d, want 1", comments)
	}
}

func assertAdversarialParityOutcome(t *testing.T, tt dispatchParityAdversarialCase, state string, dispatches int) {
	t.Helper()
	if state != tt.WantState {
		t.Fatalf("state = %q, want %q", state, tt.WantState)
	}
	if dispatches > tt.WantMaxDispatches {
		t.Fatalf("dispatches = %d, max %d", dispatches, tt.WantMaxDispatches)
	}
}

type parityRateLimitConnector struct {
	*soakConnector
	resetAt time.Time
}

func (c *parityRateLimitConnector) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	return connector.RESTRateLimitUsage{
		HasRateLimit: true,
		RateLimited:  true,
		RateLimit: connector.RESTRateLimit{
			Limit:     5_000,
			Used:      5_000,
			Remaining: 0,
			Resource:  "core",
			ResetAt:   c.resetAt,
		},
	}
}

type dispatchParityFixture struct {
	Source           string                          `json:"source"`
	Cases            []dispatchParityCase            `json:"cases"`
	AdversarialCases []dispatchParityAdversarialCase `json:"adversarial_cases"`
}

type dispatchParityAdversarialCase struct {
	Name              string `json:"name"`
	Behavior          string `json:"behavior"`
	Now               string `json:"now"`
	SimulatedTicks    int    `json:"simulated_ticks"`
	WantState         string `json:"want_state"`
	WantMaxDispatches int    `json:"want_max_dispatches"`
}

type dispatchParityCase struct {
	Name         string             `json:"name"`
	Now          string             `json:"now"`
	Config       dispatchParityCfg  `json:"config"`
	InitialState parityInitialState `json:"initial_state"`
	Candidates   []parityIssue      `json:"candidates"`
	Want         parityWant         `json:"want"`
}

type dispatchParityCfg struct {
	MaxConcurrentAgents          int            `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState   map[string]int `json:"max_concurrent_agents_by_state"`
	DispatchPriorityByState      []string       `json:"dispatch_priority_by_state"`
	DispatchPriorityByLabel      []string       `json:"dispatch_priority_by_label"`
	ActiveStates                 []string       `json:"active_states"`
	TerminalStates               []string       `json:"terminal_states"`
	BudgetRefusalCooldownSeconds int            `json:"budget_refusal_cooldown_seconds"`
	MaxConcurrentAgentsPerHost   int            `json:"max_concurrent_agents_per_host"`
	WorkerHosts                  []string       `json:"worker_hosts"`
}

type parityInitialState struct {
	Running        []parityRunning       `json:"running"`
	Claimed        []parityClaimed       `json:"claimed"`
	Blocked        []parityBlocked       `json:"blocked"`
	BudgetRefusals []parityBudgetRefusal `json:"budget_refusals"`
}

type parityRunning struct {
	Issue      parityIssue `json:"issue"`
	WorkerHost string      `json:"worker_host"`
}

type parityClaimed struct {
	Issue parityIssue `json:"issue"`
}

type parityBlocked struct {
	Issue  parityIssue `json:"issue"`
	Reason string      `json:"reason"`
}

type parityBudgetRefusal struct {
	Issue     parityIssue `json:"issue"`
	RefusedAt string      `json:"refused_at"`
	ResetAt   string      `json:"reset_at"`
}

type parityIssue struct {
	ID               string                 `json:"id"`
	Identifier       string                 `json:"identifier"`
	Title            string                 `json:"title"`
	State            string                 `json:"state"`
	Priority         *int                   `json:"priority"`
	CreatedAt        string                 `json:"created_at"`
	BlockedBy        []connector.BlockedRef `json:"blocked_by"`
	AssignedToWorker *bool                  `json:"assigned_to_worker"`
}

type parityWant struct {
	DispatchOrder []string `json:"dispatch_order"`
	Blocked       []string `json:"blocked"`
	Claimed       []string `json:"claimed"`
	Refusals      []string `json:"refusals"`
}

type parityBlockingRunner struct{}

func (parityBlockingRunner) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

func loadDispatchParityFixture(t *testing.T) dispatchParityFixture {
	t.Helper()

	path := filepath.Join("testdata", "elixir_dispatch_parity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var fixture dispatchParityFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatalf("%s contains no parity cases", path)
	}
	return fixture
}

func applyParityInitialState(t *testing.T, state *State, initial parityInitialState, now time.Time) {
	t.Helper()

	for _, running := range initial.Running {
		issue := parityConnectorIssue(t, running.Issue)
		state.Running[issue.ID] = Running{
			Issue:      issue,
			StartedAt:  now,
			WorkerHost: running.WorkerHost,
		}
	}
	for _, claimed := range initial.Claimed {
		issue := parityConnectorIssue(t, claimed.Issue)
		state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now}
	}
	for _, blocked := range initial.Blocked {
		issue := parityConnectorIssue(t, blocked.Issue)
		state.Blocked[issue.ID] = Blocked{Issue: issue, Reason: blocked.Reason, BlockedAt: now}
	}
	for _, refusal := range initial.BudgetRefusals {
		issue := parityConnectorIssue(t, refusal.Issue)
		state.BudgetRefusals[issue.ID] = BudgetRefusal{
			Issue:     issue,
			RefusedAt: mustParseParityTime(t, refusal.RefusedAt),
			ResetAt:   optionalParityTime(t, refusal.ResetAt),
		}
	}
}

func parityIssues(t *testing.T, issues []parityIssue) []connector.Issue {
	t.Helper()

	got := make([]connector.Issue, len(issues))
	for i, issue := range issues {
		got[i] = parityConnectorIssue(t, issue)
	}
	return got
}

func parityConnectorIssue(t *testing.T, input parityIssue) connector.Issue {
	t.Helper()

	issue := connector.NewIssue()
	issue.ID = input.ID
	issue.Identifier = input.Identifier
	issue.Title = input.Title
	issue.State = input.State
	issue.Priority = input.Priority
	issue.BlockedBy = append([]connector.BlockedRef(nil), input.BlockedBy...)
	if input.CreatedAt != "" {
		createdAt := mustParseParityTime(t, input.CreatedAt)
		issue.CreatedAt = &createdAt
	}
	if input.AssignedToWorker != nil {
		issue.AssignedToWorker = *input.AssignedToWorker
	}
	return issue
}

func parityDispatchOrder(orch *Orchestrator, state State, candidates []connector.Issue, now time.Time) []string {
	order := make([]string, 0)
	for _, issue := range candidates {
		if availableSlots(&state) == 0 {
			return order
		}
		if !orch.dispatchable(issue, &state, now) {
			continue
		}

		workerHost, ok := orch.selectWorkerHost(&state, "")
		if !ok {
			continue
		}

		issue = cloneIssue(issue)
		order = append(order, issue.ID)
		state.Running[issue.ID] = Running{Issue: issue, StartedAt: now, WorkerHost: workerHost}
		state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now}
		delete(state.Retry, issue.ID)
		delete(state.Blocked, issue.ID)
		delete(state.BudgetRefusals, issue.ID)
	}
	return order
}

func assertParitySet(t *testing.T, name string, got []string, want []string) {
	t.Helper()

	sortedGot := sortedParityStrings(got)
	sortedWant := sortedParityStrings(want)
	if !slices.Equal(sortedGot, sortedWant) {
		t.Fatalf("%s = %#v, want %#v", name, sortedGot, sortedWant)
	}
}

func sortedParityStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

func stateIDs[T any](items map[string]T) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	return ids
}

func mustParseParityTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func optionalParityTime(t *testing.T, value string) *time.Time {
	t.Helper()

	if value == "" {
		return nil
	}
	parsed := mustParseParityTime(t, value)
	return &parsed
}
