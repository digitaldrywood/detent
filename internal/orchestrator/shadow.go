package orchestrator

import (
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type DispatchDecision struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier,omitempty"`
	State      string `json:"state,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	WorkerHost string `json:"worker_host,omitempty"`
	Retry      bool   `json:"retry,omitempty"`
}

type DispatchPlan struct {
	Dispatches     []DispatchDecision `json:"dispatches,omitempty"`
	Claimed        []string           `json:"claimed,omitempty"`
	Blocked        []string           `json:"blocked,omitempty"`
	BudgetRefusals []string           `json:"budget_refusals,omitempty"`
	Retry          []string           `json:"retry,omitempty"`
}

func PlanDispatch(cfg Config, state State, candidates []connector.Issue, now time.Time) DispatchPlan {
	planner := newDispatchPlanner(cfg)
	if now.IsZero() {
		now = time.Now().UTC()
	}

	plannedState := state.clone()
	plannedState.ensureInitialized(planner.cfg)
	plannedCandidates := cloneIssues(candidates)
	planner.pruneBudgetRefusals(&plannedState, now)
	planner.trackBlockedCandidates(&plannedState, plannedCandidates, now)

	return planner.plan(&plannedState, plannedCandidates, now, dispatchPlanHooks{})
}

func (p DispatchPlan) DispatchOrder() []string {
	order := make([]string, 0, len(p.Dispatches))
	for _, dispatch := range p.Dispatches {
		order = append(order, dispatch.IssueID)
	}
	return order
}

func (s *State) ensureInitialized(cfg Config) {
	if s.PollInterval <= 0 {
		s.PollInterval = cfg.PollInterval
	}
	if s.MaxConcurrentAgents <= 0 {
		s.MaxConcurrentAgents = cfg.MaxConcurrentAgents
	}
	if s.Running == nil {
		s.Running = map[string]Running{}
	}
	if s.Claimed == nil {
		s.Claimed = map[string]Claimed{}
	}
	if s.Blocked == nil {
		s.Blocked = map[string]Blocked{}
	}
	if s.Completed == nil {
		s.Completed = map[string]Completed{}
	}
	if s.Retry == nil {
		s.Retry = map[string]Retry{}
	}
	if s.BudgetRefusals == nil {
		s.BudgetRefusals = map[string]BudgetRefusal{}
	}
	if s.DiffStats == nil {
		s.DiffStats = map[string]DiffStats{}
	}
}

func claimedIDs(values map[string]Claimed) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func blockedIDs(values map[string]Blocked) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func budgetRefusalIDs(values map[string]BudgetRefusal) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func retryIDs(values map[string]Retry) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
