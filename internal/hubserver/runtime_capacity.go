package hubserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRuntimeRunners(ctx context.Context, query nativeQueryer, scope nativeScope, now time.Time) ([]runnerauth.Runner, bool, error) {
	limit := 100
	truncated := false
	if explanationRead(ctx) {
		limit = 16
	}
	runnerWhere := ` WHERE r.organization_id = ? AND r.removed_at IS NULL AND EXISTS
(SELECT 1 FROM token_grants g WHERE g.token_id = r.token_id AND g.organization_id = r.organization_id AND g.project_id = ?)`
	statement := runnerIdentitySelect + runnerWhere + ` ORDER BY CASE WHEN r.id = ? THEN 0 ELSE 1 END, r.id LIMIT ?`
	args := []any{scope.organization, scope.project, scope.credential.Runner.RunnerID, limit + 1}
	if explanationRead(ctx) {
		candidates, err := query.QueryContext(ctx, "SELECT id FROM runner_identities WHERE organization_id=? ORDER BY id LIMIT 17", scope.organization)
		if err != nil {
			return nil, false, err
		}
		ids := []any{}
		for candidates.Next() {
			var id string
			if err := candidates.Scan(&id); err != nil {
				candidates.Close()
				return nil, false, err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(candidates.Err(), candidates.Close()); err != nil {
			return nil, false, err
		}
		truncated = len(ids) > limit
		ids = ids[:min(len(ids), limit)]
		statement = "SELECT * FROM (" + runnerIdentitySelect + runnerWhere + " AND r.id=?) UNION ALL SELECT * FROM (" + runnerIdentitySelect + runnerWhere + " AND r.id IN (" + placeholders(len(ids)) + ") AND r.id<>? ORDER BY r.id)"
		args = []any{scope.organization, scope.project, scope.credential.Runner.RunnerID, scope.organization, scope.project}
		args = append(args, ids...)
		args = append(args, scope.credential.Runner.RunnerID)
	}

	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	runners := []runnerauth.Runner{}
	reports := make(map[string][]providercapacity.Report)
	hosts := make(map[tracker.MachineID]int)
	var machines []any
	for rows.Next() {
		if len(runners) == limit {
			truncated = true
			break
		}
		r, providerReports, _, err := scanRunnerIdentity(rows, func() time.Time { return now })
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
	leaseStatement := `SELECT l.machine_id, coalesce(lr.runner_id, ''), l.expires_at, l.session_id, l.lease_id, coalesce(i.project_id, ''), ` + nativeLandingCapacityProject + `
FROM leases l JOIN issues i ON i.id = l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id ` + nativeLandingCapacityJoins + `
WHERE l.machine_id IN (` + placeholders(len(machines)) + `) AND l.released_at IS NULL`
	leaseArgs := machines
	if explanationRead(ctx) {
		leaseStatement = `SELECT l.machine_id,coalesce(lr.runner_id,''),l.expires_at,l.session_id,l.lease_id,coalesce(i.project_id,''),''
FROM leases l INDEXED BY health_active_host JOIN issues i ON i.id=l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
WHERE l.machine_id IN (` + placeholders(len(machines)) + `) AND l.released_at IS NULL AND l.expires_at>? ORDER BY l.machine_id,l.expires_at,l.lease_id LIMIT 1025`
		leaseArgs = append(append([]any(nil), machines...), formatHubTime(now))
	}
	rows, err = query.QueryContext(ctx, leaseStatement, leaseArgs...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	used := make(map[tracker.MachineID]map[string]int)
	projects := make(map[string]map[tracker.ProjectID]int)
	hostSlots := make(map[string]bool)
	runnerSlots := make(map[string]bool)
	leaseCount := 0
	for rows.Next() {
		leaseCount++
		if explanationRead(ctx) && leaseCount > 1024 {
			truncated = true
			break
		}
		var machine tracker.MachineID
		var runner, expiry, session, lease, batchProject string
		var project tracker.ProjectID
		if err := rows.Scan(&machine, &runner, &expiry, &session, &lease, &project, &batchProject); err != nil {
			return nil, false, err
		}
		if explanationRead(ctx) && strings.HasPrefix(session, "landing/") {
			truncated = true
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return nil, false, err
		}
		if end.After(now) {
			key := landingCapacityKey(machine, batchProject, session)
			if key == "" {
				key = lease
			}
			if !hostSlots[key] {
				hosts[machine]++
				hostSlots[key] = true
			}
			if used[machine] == nil {
				used[machine] = make(map[string]int)
			}
			if !runnerSlots[runner+"/"+key] {
				used[machine][runner]++
				if projects[runner] == nil {
					projects[runner] = make(map[tracker.ProjectID]int)
				}
				projects[runner][project]++
				runnerSlots[runner+"/"+key] = true
			}
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
	truncated = truncated || provider.truncated
	for i := range runners {
		r := &runners[i]
		r.HostUsed, r.Used = hosts[r.MachineID], used[r.MachineID][r.RunnerID]
		r.ProjectUsed = projects[r.RunnerID]
		for _, report := range reports[r.RunnerID] {
			r.ProviderCapacity = append(r.ProviderCapacity, provider.view(r.RunnerID, report, now))
		}
	}
	return runners, truncated, nil
}
