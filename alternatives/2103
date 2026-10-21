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
