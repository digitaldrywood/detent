package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runnerHubProblems(r runnerauth.Runner, report isolation.Report, protocol int, rejected bool) []runnerauth.Problem {
	problems := []runnerauth.Problem{}
	if rejected {
		problems = append(problems, runnerauth.NewProblem("settings_rejected"))
	}
	if protocol != 0 && protocol != 2 {
		problems = append(problems, runnerauth.NewProblem("version_unsupported"))
	}
	return problems
}

func applyRunnerProblems(r *runnerauth.Runner, raw, isolationRaw string, protocol int, rejected bool) error {
	var previous []runnerauth.Problem
	var report isolation.Report
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(isolationRaw), &report); err != nil {
		return err
	}
	r.BackendIsolation = report
	current := slices.DeleteFunc(slices.Clone(previous), func(p runnerauth.Problem) bool {
		return p.Code == "settings_rejected" || p.Code == "version_unsupported" || p.Code == "policy_mismatch"
	})
	current = append(current, runnerHubProblems(*r, report, protocol, rejected)...)
	r.Problems = runnerauth.MergeProblems(previous, current, r.LastHeartbeatAt)
	if len(r.Problems) > 0 && r.Health != "revoked" && r.Health != "expired" {
		r.Health = "needs_attention"
	}
	return nil
}

func applyRunnerPolicyProblems(ctx context.Context, q policyQuerier, r *runnerauth.Runner) error {
	var raw, configurationRaw, admissionRaw string
	if err := q.QueryRowContext(ctx, `SELECT r.problems_json, r.project_configuration_json,
coalesce(json_extract(m.capabilities_json, '$.native_admission.' || r.id), '{}')
FROM runner_identities r JOIN machines m ON m.id = r.machine_id WHERE r.id = ? AND r.organization_id = ?`, r.RunnerID, r.OrganizationID).Scan(&raw, &configurationRaw, &admissionRaw); err != nil {
		return err
	}
	var previous []runnerauth.Problem
	var configurations map[string]runnerauth.ProjectConfiguration
	var admissions map[string]tracker.NativeAdmissionObservation
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(configurationRaw), &configurations); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(admissionRaw), &admissions); err != nil {
		return err
	}
	current := slices.DeleteFunc(slices.Clone(r.Problems), func(p runnerauth.Problem) bool { return p.Code == "policy_mismatch" })
	for _, projectID := range r.ProjectIDs {
		scope := string(r.OrganizationID) + "/" + string(projectID)
		id := admissions[string(projectID)].Context.PolicyID
		if effective := configurations[string(projectID)].EffectivePolicy; effective != nil {
			id = effective.ID
		}
		if id == "" {
			err := q.QueryRowContext(ctx, "SELECT policy_id FROM project_observed_policies WHERE scope = ? AND runner_id = ?", scope, r.RunnerID).Scan(&id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if id == "" {
			continue
		}
		approved, err := readProjectPolicy(ctx, q, scope)
		var refusal *nativeError
		if err != nil && (!errors.As(err, &refusal) || refusal.Code != "policy_mismatch") {
			return err
		}
		matches := false
		if approved.Policy.ID != "" {
			matches, err = approvedPolicyRevisionMatches(ctx, q, scope, id, approved.Policy)
			if err != nil {
				return err
			}
		}
		if matches {
			continue
		}
		problem := runnerauth.NewProblem("policy_mismatch")
		problem.ProjectID = string(projectID)
		approvedID := approved.Policy.ID
		if approvedID == "" {
			approvedID = "none"
		}
		problem.Message = fmt.Sprintf("Project %s: Hub approved policy %s does not permit runner policy %s.", projectID, approvedID, id)
		current = append(current, problem)
	}
	r.Problems = runnerauth.MergeProblems(previous, current, r.LastHeartbeatAt)
	if len(r.Problems) > 0 && r.Health != "revoked" && r.Health != "expired" {
		r.Health = "needs_attention"
	}
	return nil
}

func updateRunnerProblems(ctx context.Context, tx *sql.Tx, scope nativeScope, problems []runnerauth.Problem, protocol int, rejected bool, now time.Time) error {
	if err := runnerauth.ValidateReportedProblems(problems); err != nil {
		return nativeInvalid(err.Error())
	}
	// Problems derive from the runner's own row; the full runner view also
	// hydrates leases and organization-wide provider capacity, which the
	// heartbeat must not read while it holds the writer.
	r, _, _, err := scanRunnerIdentity(tx.QueryRowContext(ctx, runnerIdentitySelect+" WHERE r.organization_id = ? AND r.id = ?", scope.organization, scope.credential.Runner.RunnerID), func() time.Time { return now })
	if err != nil {
		return err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT backend_isolation_json FROM runner_identities WHERE id = ?", r.RunnerID).Scan(&raw); err != nil {
		return err
	}
	var report isolation.Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return err
	}
	for i := range problems {
		problems[i] = runnerauth.SanitizeProblem(problems[i])
		problems[i].ReportedAt = now
	}
	current := append(slices.Clone(problems), runnerHubProblems(r, report, protocol, rejected)...)
	if err := applyRunnerPolicyProblems(ctx, tx, &r); err != nil {
		return err
	}
	for _, problem := range r.Problems {
		if problem.Code == "policy_mismatch" {
			current = append(current, problem)
		}
	}
	encoded, err := json.Marshal(runnerauth.MergeProblems(r.Problems, current, now))
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE runner_identities SET problems_json = ?, reported_protocol_major = ?, settings_rejected = ? WHERE id = ? AND organization_id = ? AND token_id = ?", string(encoded), protocol, rejected, r.RunnerID, scope.organization, scope.credential.ID)
	return requireRunnerUpdate(result, err)
}

func refreshRunnerProblems(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, id string, now time.Time) error {
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT problems_json FROM runner_identities WHERE id = ? AND organization_id = ?", id, organization).Scan(&raw); err != nil {
		return err
	}
	var previous []runnerauth.Problem
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	r, err := readRunner(ctx, tx, organization, id, now)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(runnerauth.MergeProblems(previous, r.Problems, now))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET problems_json = ? WHERE id = ? AND organization_id = ?", string(encoded), id, organization)
	return err
}
