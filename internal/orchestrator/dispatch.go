package orchestrator

import (
	"strings"
	"time"
)

func (o *Orchestrator) pruneBudgetRefusals(state *State, now time.Time) {
	for issueID, refusal := range state.BudgetRefusals {
		if !o.budgetRefusalActive(refusal, now) {
			delete(state.BudgetRefusals, issueID)
		}
	}
}

func (o *Orchestrator) budgetCooldownActive(state *State, issueID string, now time.Time) bool {
	refusal, ok := state.BudgetRefusals[issueID]
	if !ok {
		return false
	}

	return o.budgetRefusalActive(refusal, now)
}

func (o *Orchestrator) budgetRefusalActive(refusal BudgetRefusal, now time.Time) bool {
	if refusal.ResetAt != nil && now.Before(*refusal.ResetAt) {
		return true
	}
	if o.cfg.BudgetRefusalCooldown <= 0 || refusal.RefusedAt.IsZero() {
		return false
	}

	return now.Before(refusal.RefusedAt.Add(o.cfg.BudgetRefusalCooldown))
}

func (o *Orchestrator) workerSlotsAvailable(state *State, preferredWorkerHost string) bool {
	_, ok := o.selectWorkerHost(state, preferredWorkerHost)
	return ok
}

func (o *Orchestrator) selectWorkerHost(state *State, preferredWorkerHost string) (string, bool) {
	if len(o.cfg.WorkerHosts) == 0 {
		return "", true
	}

	availableHosts := make([]string, 0, len(o.cfg.WorkerHosts))
	for _, host := range o.cfg.WorkerHosts {
		if o.workerHostSlotsAvailable(state, host) {
			availableHosts = append(availableHosts, host)
		}
	}
	if len(availableHosts) == 0 {
		return "", false
	}

	preferredWorkerHost = strings.TrimSpace(preferredWorkerHost)
	if preferredWorkerHost != "" {
		for _, host := range availableHosts {
			if host == preferredWorkerHost {
				return preferredWorkerHost, true
			}
		}
	}

	return leastLoadedWorkerHost(state, availableHosts), true
}

func (o *Orchestrator) workerHostSlotsAvailable(state *State, workerHost string) bool {
	if o.cfg.MaxConcurrentAgentsPerHost <= 0 {
		return true
	}

	return runningWorkerHostCount(state, workerHost) < o.cfg.MaxConcurrentAgentsPerHost
}

func leastLoadedWorkerHost(state *State, hosts []string) string {
	selected := hosts[0]
	selectedCount := runningWorkerHostCount(state, selected)
	for _, host := range hosts[1:] {
		count := runningWorkerHostCount(state, host)
		if count < selectedCount {
			selected = host
			selectedCount = count
		}
	}
	return selected
}

func runningWorkerHostCount(state *State, workerHost string) int {
	count := 0
	for _, running := range state.Running {
		if running.WorkerHost == workerHost {
			count++
		}
	}
	return count
}

func normalizeWorkerHosts(hosts []string) []string {
	normalized := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		normalized = append(normalized, host)
	}
	return normalized
}
