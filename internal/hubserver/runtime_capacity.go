package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRuntimeRunners(ctx context.Context, query nativeQueryer, scope nativeScope, now time.Time) ([]runnerauth.Runner, bool, error) {
	rows, err := query.QueryContext(ctx, runnerIdentitySelect+` WHERE r.organization_id = ? AND EXISTS
(SELECT 1 FROM token_grants g WHERE g.token_id = r.token_id AND g.organization_id = r.organization_id AND g.project_id = ?)
ORDER BY CASE WHEN r.id = ? THEN 0 ELSE 1 END, r.id LIMIT 101`, scope.organization, scope.project, scope.credential.Runner.RunnerID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	runners := []runnerauth.Runner{}
	reports := make(map[string][]providercapacity.Report)
	hosts := make(map[tracker.MachineID]int)
	var machines []any
	truncated := false
	for rows.Next() {
		if len(runners) == 100 {
			truncated = true
			break
		}
		r, providerReports, err := scanRunnerIdentity(rows, now)
		if err != nil {
			return nil, false, err
		}
		runners = append(runners, r)
		reports[r.RunnerID] = providerReports
		if _, found := hosts[r.MachineID]; !found {
			hosts[r.MachineID] = 0
			machines = append(machines, r.MachineID)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(runners) == 0 {
		return runners, truncated, nil
	}
	rows, err = query.QueryContext(ctx, `SELECT l.machine_id, coalesce(lr.runner_id, ''), l.expires_at
FROM leases l JOIN issues i ON i.id = l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id
WHERE l.machine_id IN (`+placeholders(len(machines))+`) AND l.released_at IS NULL`, machines...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	used := make(map[tracker.MachineID]map[string]int)
	for rows.Next() {
		var machine tracker.MachineID
		var runner, expiry string
		if err := rows.Scan(&machine, &runner, &expiry); err != nil {
			return nil, false, err
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return nil, false, err
		}
		if end.After(now) {
			hosts[machine]++
			if used[machine] == nil {
				used[machine] = make(map[string]int)
			}
			used[machine][runner]++
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	var provider providerCapacitySnapshot
	for _, r := range runners {
		if len(reports[r.RunnerID]) > 0 {
			provider, err = readProviderCapacitySnapshot(ctx, query, scope.organization, now)
			if err != nil {
				return nil, false, err
			}
			break
		}
	}
	for i := range runners {
		r := &runners[i]
		r.HostUsed, r.Used = hosts[r.MachineID], used[r.MachineID][r.RunnerID]
		for _, report := range reports[r.RunnerID] {
			r.ProviderCapacity = append(r.ProviderCapacity, provider.view(report, now))
		}
	}
	return runners, truncated, nil
}
