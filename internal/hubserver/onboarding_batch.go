package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) projectOnboardingBatch(ctx context.Context, organization tracker.OrganizationID, projects []appBootstrapProject) (map[tracker.ProjectID]onboarding.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	result := make(map[tracker.ProjectID]onboarding.Project, len(projects))
	if len(projects) == 0 {
		return result, nil
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT p.id, coalesce(nullif(p.checkout_repository,''),r.github_owner||'/'||r.github_name,''), coalesce(o.progress_json,'{}'),
 coalesce((SELECT json_group_array(json(binding_json)) FROM artifact_services a WHERE a.organization_id=p.organization_id AND a.project_id=p.id),'[]')
 FROM projects p LEFT JOIN repositories r ON r.id=p.repository_id LEFT JOIN project_onboarding o ON o.organization_id=p.organization_id AND o.project_id=p.id
 WHERE p.organization_id=? AND p.deleted_at IS NULL AND p.id IN (SELECT value FROM json_each(?))`, organization, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.ProjectID
		var progress, artifacts string
		value := onboarding.Project{Runners: []runnerauth.Eligibility{}, Artifacts: []artifact.Binding{}}
		if err := rows.Scan(&id, &value.Repository, &progress, &artifacts); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if err := json.Unmarshal([]byte(progress), &value.Progress); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if err := json.Unmarshal([]byte(artifacts), &value.Artifacts); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		result[id] = value
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	approvals := map[tracker.ProjectID]policy.Approval{}
	rows, err = tx.QueryContext(ctx, `SELECT p.id, r.policy_id, r.metadata_json, r.approved_by, r.approved_at, coalesce(pp.policy_id,'')=r.policy_id
 FROM policy_revisions r JOIN projects p ON r.scope=p.organization_id||'/'||p.id
 LEFT JOIN project_policies pp ON pp.scope=r.scope WHERE p.organization_id=?`, organization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.ProjectID
		var policyID, raw string
		var approval policy.Approval
		var current bool
		if err := rows.Scan(&id, &policyID, &raw, &approval.ApprovedBy, &approval.ApprovedAt, &current); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if current {
			approval, err = decodeProjectPolicy(raw, approval)
			if err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			approvals[id] = approval
			if value, ok := result[id]; ok {
				value.Policy = &approval
				result[id] = value
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	now := s.config.now()
	capacity, err := readProviderCapacitySnapshot(ctx, tx, organization, now)
	if err != nil {
		return nil, err
	}
	machineUsed := map[tracker.MachineID]int{}
	runnerUsed := map[string]int{}
	rows, err = tx.QueryContext(ctx, `SELECT l.machine_id,coalesce(lr.runner_id,'') FROM leases l
 JOIN issues i ON i.id=l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
 WHERE i.organization_id=? AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)`, organization, formatHubTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var machine tracker.MachineID
		var runner string
		if err := rows.Scan(&machine, &runner); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		machineUsed[machine]++
		runnerUsed[runner]++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	query := strings.Replace(runnerIdentitySelect, "\nFROM runner_identities", `, r.local_checks_json
 FROM runner_identities`, 1)
	rows, err = tx.QueryContext(ctx, query+" WHERE r.organization_id=? AND r.removed_at IS NULL ORDER BY r.display_name,r.id", organization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var checksRaw string
		runner, reports, _, err := scanRunnerIdentity(rows, func() time.Time { return now }, &checksRaw)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		runner.HostUsed = machineUsed[runner.MachineID]
		runner.Used = runnerUsed[runner.RunnerID]
		for _, report := range reports {
			runner.ProviderCapacity = append(runner.ProviderCapacity, capacity.view(runner.RunnerID, report, now))
		}
		var checks map[tracker.ProjectID]*runnerauth.LocalChecks
		if err := json.Unmarshal([]byte(checksRaw), &checks); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		for id, value := range result {
			if !runner.AllowsProject(id) {
				continue
			}
			value.Runners = append(value.Runners, runnerauth.Eligibility{Runner: runner, Exclusions: runner.Exclusions(id, approvals[id].Policy.Requirements, false), LocalChecks: checks[id]})
			result[id] = value
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for id, value := range result {
		value.Evaluate()
		result[id] = value
	}
	return result, nil
}
