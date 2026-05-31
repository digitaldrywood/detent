package orchestrator

import (
	"sort"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/connector"
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
	cfg = normalizeConfig(cfg)
	if now.IsZero() {
		now = time.Now().UTC()
	}

	plannedState := state.clone()
	plannedState.ensureInitialized(cfg)
	plannedCandidates := cloneIssues(candidates)
	sortIssuesForDispatch(plannedCandidates, cfg.DispatchPriorityByState)

	orch := Orchestrator{cfg: cfg}
	orch.pruneBudgetRefusals(&plannedState, now)
	orch.trackBlockedCandidates(&plannedState, plannedCandidates, now)
	dueRetries := dueRetriesByIssue(&plannedState, now)
	orch.releaseMissingDueRetries(&plannedState, plannedCandidates, dueRetries)

	dispatches := make([]DispatchDecision, 0)
	for _, issue := range plannedCandidates {
		if retry, ok := dueRetries[issue.ID]; ok {
			if decision, ok := orch.planRetryIssue(&plannedState, issue, retry, now); ok {
				dispatches = append(dispatches, decision)
			}
			continue
		}
		if availableSlots(&plannedState) == 0 {
			break
		}
		if !orch.dispatchable(issue, &plannedState, now) {
			continue
		}
		if decision, ok := orch.planDispatchIssue(&plannedState, issue, 0, now, ""); ok {
			dispatches = append(dispatches, decision)
		}
	}

	return DispatchPlan{
		Dispatches:     dispatches,
		Claimed:        claimedIDs(plannedState.Claimed),
		Blocked:        blockedIDs(plannedState.Blocked),
		BudgetRefusals: budgetRefusalIDs(plannedState.BudgetRefusals),
		Retry:          retryIDs(plannedState.Retry),
	}
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

func (o *Orchestrator) planRetryIssue(
	state *State,
	issue connector.Issue,
	retry Retry,
	now time.Time,
) (DispatchDecision, bool) {
	delete(state.Retry, retry.Issue.ID)

	if !o.dispatchableForRetry(issue, state, now, retry.WorkerHost) {
		if o.budgetCooldownActive(state, issue.ID, now) {
			o.scheduleRetry(state, issue, retry.Attempt, now, "budget cooldown active", false, retry.WorkerHost)
			return DispatchDecision{}, false
		}
		if !o.slotsAvailable(issue, state, retry.WorkerHost) {
			o.scheduleRetry(state, issue, retry.Attempt, now, "no available orchestrator slots", false, retry.WorkerHost)
			return DispatchDecision{}, false
		}
		if _, blocked := state.Blocked[issue.ID]; blocked {
			o.releaseClaim(state, issue.ID)
			return DispatchDecision{}, false
		}

		o.releaseIssue(state, issue.ID)
		return DispatchDecision{}, false
	}

	return o.planDispatchIssue(state, issue, retry.Attempt, now, retry.WorkerHost)
}

func (o *Orchestrator) planDispatchIssue(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	preferredWorkerHost string,
) (DispatchDecision, bool) {
	workerHost, ok := o.selectWorkerHost(state, preferredWorkerHost)
	if !ok {
		return DispatchDecision{}, false
	}

	issue = cloneIssue(issue)
	state.Running[issue.ID] = Running{
		Issue:      issue,
		Attempt:    attempt,
		StartedAt:  now,
		WorkerHost: workerHost,
	}
	state.Claimed[issue.ID] = Claimed{
		Issue:     issue,
		ClaimedAt: now,
	}
	delete(state.Retry, issue.ID)
	delete(state.Blocked, issue.ID)
	delete(state.BudgetRefusals, issue.ID)

	return DispatchDecision{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		State:      issue.State,
		Attempt:    attempt,
		WorkerHost: workerHost,
		Retry:      attempt > 0,
	}, true
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
