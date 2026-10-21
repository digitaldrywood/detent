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

const cachedProbeProblemWindow = 2 * time.Minute

func runnerHubProblems(protocol int, rejected bool) []runnerauth.Problem {
	problems := []runnerauth.Problem{}
	if rejected {
		problems = append(problems, runnerauth.NewProblem("settings_rejected"))
	}
	if protocol != 0 && protocol != 2 {
		problems = append(problems, runnerauth.NewProblem("version_unsupported"))
	}
	return problems
}

func applyRunnerProblems(r *runnerauth.Runner, raw, isolationRaw string, protocol int, rejected bool, now time.Time) error {
	var previous []runnerauth.Problem
	var report isolation.Report
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(isolationRaw), &report); err != nil {
		return err
	}
	r.BackendIsolation = report
	r.Problems = mergeRunnerProblems(previous, previous, protocol, rejected, r.LastHeartbeatAt)
	r.Problems = slices.DeleteFunc(r.Problems, func(p runnerauth.Problem) bool {
		return p.Code == "backend_probe_failed" && p.CachedCapabilities && p.Subject == "backend_isolation" && p.Check == "runner isolation probe" && !p.FirstSeen.IsZero() && now.Before(p.FirstSeen.Add(cachedProbeProblemWindow))
	})
	if len(r.Problems) > 0 && r.Health != "revoked" && r.Health != "expired" {
		r.Health = "needs_attention"
	}
	return nil
}

func updateRunnerProblems(ctx context.Context, tx *sql.Tx, scope nativeScope, problems []runnerauth.Problem, protocol int, rejected bool, now time.Time) error {
	if err := runnerauth.ValidateReportedProblems(problems); err != nil {
		return nativeInvalid(err.Error())
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT problems_json FROM runner_identities WHERE id = ? AND organization_id = ? AND token_id = ?", scope.credential.Runner.RunnerID, scope.organization, scope.credential.ID).Scan(&raw); err != nil {
		return err
	}
	var previous []runnerauth.Problem
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	for i := range problems {
		problems[i] = runnerauth.SanitizeProblem(problems[i])
		if problems[i].ReportedAt.IsZero() {
			problems[i].ReportedAt = now
		}
	}
	encoded, err := json.Marshal(mergeRunnerProblems(previous, problems, protocol, rejected, now))
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE runner_identities SET problems_json = ?, reported_protocol_major = ?, settings_rejected = ? WHERE id = ? AND organization_id = ? AND token_id = ?", string(encoded), protocol, rejected, scope.credential.Runner.RunnerID, scope.organization, scope.credential.ID)
	return requireRunnerUpdate(result, err)
}

func refreshRunnerProblems(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, id string, now time.Time) error {
	var raw string
	var protocol int
	var rejected bool
	if err := tx.QueryRowContext(ctx, "SELECT problems_json, reported_protocol_major, settings_rejected FROM runner_identities WHERE id = ? AND organization_id = ?", id, organization).Scan(&raw, &protocol, &rejected); err != nil {
		return err
	}
	var previous []runnerauth.Problem
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return err
	}
	encoded, err := json.Marshal(mergeRunnerProblems(previous, previous, protocol, rejected, now))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET problems_json = ? WHERE id = ? AND organization_id = ?", string(encoded), id, organization)
	return err
}

func mergeRunnerProblems(previous, current []runnerauth.Problem, protocol int, rejected bool, now time.Time) []runnerauth.Problem {
	current = slices.DeleteFunc(slices.Clone(current), func(p runnerauth.Problem) bool {
		return p.Code == "settings_rejected" || p.Code == "version_unsupported" || p.Code == "policy_mismatch"
	})
	current = append(current, runnerHubProblems(protocol, rejected)...)
	return runnerauth.MergeProblems(previous, current, now)
}

func appendRunnerRuntimeProblems(ctx context.Context, db nativeQueryer, r *runnerauth.Runner, now time.Time) error {
	projects := make(map[tracker.LeaseID]tracker.ProjectID, len(r.Leases))
	args := []any{r.OrganizationID}
	for _, lease := range r.Leases {
		if slices.Contains(r.ProjectIDs, lease.ProjectID) {
			projects[lease.ID] = lease.ProjectID
			args = append(args, lease.ID)
		}
	}
	if len(projects) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT a.data_json, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at, l.renewed_at, a.work_item_revision, a.dispatch_generation
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND l.lease_id IN (`+placeholders(len(projects))+`) AND a.status = 'running' AND l.released_at IS NULL`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		attempt, err := scanNativeAttempt(rows, now)
		if err != nil {
			return err
		}
		project, visible := projects[attempt.LeaseID]
		if !visible || !attempt.Current || attempt.RuntimeFreshness != "expired" || attempt.Runtime == nil ||
			!attempt.LeaseRenewedAt.After(attempt.Runtime.HeartbeatAt) {
			continue
		}
		r.Problems = append(r.Problems, runnerauth.Problem{
			Code: "runtime_expired", ProjectID: string(project), Subject: attempt.AttemptID,
			Message:   "A running attempt has expired runtime activity while its lease is still being renewed.",
			FixHint:   "Inspect the attempt's runtime and finalization diagnostics on its runner.",
			FirstSeen: attempt.Runtime.HeartbeatAt.Add(attempt.LeaseExpiresAt.Sub(attempt.LeaseRenewedAt)), ReportedAt: attempt.LeaseRenewedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(r.Problems) > 0 && r.Health != "revoked" && r.Health != "expired" {
		r.Health = "needs_attention"
	}
	return nil
}
