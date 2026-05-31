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

	"github.com/digitaldrywood/symphony/internal/connector"
	runpkg "github.com/digitaldrywood/symphony/internal/runner"
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
			sortIssuesForDispatch(candidates, cfg.DispatchPriorityByState)
			orch.pruneBudgetRefusals(&state, now)
			orch.trackBlockedCandidates(&state, candidates, now)

			gotOrder := parityDispatchOrder(&orch, state.clone(), candidates, now)
			if !slices.Equal(gotOrder, tt.Want.DispatchOrder) {
				t.Fatalf("dispatch order = %#v, want %#v", gotOrder, tt.Want.DispatchOrder)
			}

			ctx, cancel := context.WithCancel(context.Background())
			orch.dispatchReadyIssues(ctx, &state, candidates, now)
			cancel()

			assertParitySet(t, "blocked", stateIDs(state.Blocked), tt.Want.Blocked)
			assertParitySet(t, "claimed", stateIDs(state.Claimed), tt.Want.Claimed)
			assertParitySet(t, "refusals", stateIDs(state.BudgetRefusals), tt.Want.Refusals)
		})
	}
}

type dispatchParityFixture struct {
	Source string               `json:"source"`
	Cases  []dispatchParityCase `json:"cases"`
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
