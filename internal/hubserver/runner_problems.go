package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
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

func applyRunnerProblems(r *runnerauth.Runner, raw, isolationRaw, configurationRaw string, protocol int, rejected bool) error {
	var previous []runnerauth.Problem
	var report isolation.Report
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(isolationRaw), &report); err != nil {
		return err
	}
	var configurations map[string]runnerauth.ProjectConfiguration
	if err := json.Unmarshal([]byte(configurationRaw), &configurations); err != nil {
		return err
	}
	current := slices.DeleteFunc(slices.Clone(previous), func(p runnerauth.Problem) bool {
		return p.Code == "settings_rejected" || p.Code == "version_unsupported" || p.Code == "policy_mismatch"
	})
	for projectID, configuration := range configurations {
		if slices.Contains(r.ProjectIDs, tracker.ProjectID(projectID)) && configuration.SelectedPolicyDiffers() {
			current = append(current, runnerauth.NewProblem("policy_mismatch"))
			break
		}
	}
	current = append(current, runnerHubProblems(*r, report, protocol, rejected)...)
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
	current := append(slices.Clone(problems), runnerHubProblems(r, report, protocol, rejected)...)
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
