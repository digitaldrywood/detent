package orchestrator

import (
	"strings"
	"time"
)

func (o *Orchestrator) dispatchPlanner() dispatchPlanner {
	return newDispatchPlanner(o.cfg)
}

func (o *Orchestrator) pruneBudgetRefusals(state *State, now time.Time) {
	o.dispatchPlanner().pruneBudgetRefusals(state, now)
}

func (o *Orchestrator) selectWorkerHost(state *State, preferredWorkerHost string) (string, bool) {
	return o.dispatchPlanner().selectWorkerHost(state, preferredWorkerHost)
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
