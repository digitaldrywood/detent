package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
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
	if update := r.Update; update != nil && update.Receipt != nil && update.Receipt.Applied != nil && update.Receipt.Applied.VerifiedRelease && !update.Running.Matches(*update.Receipt.Applied) {
		problem := runnerauth.NewProblem("update_applied_not_running")
		problem.Message = fmt.Sprintf("The runner control process is running %s while verified release %s is applied.", update.Running.Version, update.Receipt.Applied.Version)
		problem.ReportedAt = update.ObservedAt
		r.Problems = append(r.Problems, runnerauth.MergeProblems(nil, []runnerauth.Problem{problem}, update.Receipt.ObservedAt)...)
	}
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
	if err := decodeStorageJSON(ctx, []byte(raw), &previous); err != nil {
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
	if err := decodeStorageJSON(ctx, []byte(raw), &previous); err != nil {
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

func appendRunnerRuntimeProblems(r *runnerauth.Runner, lease runnerstore.FleetLease, now time.Time) {
	if lease.RuntimeAttemptID == "" || lease.RuntimeHeartbeatAt == nil || !slices.Contains(r.ProjectIDs, lease.Lease.ProjectID) {
		return
	}
	heartbeat := *lease.RuntimeHeartbeatAt
	ttl := lease.Lease.ExpiresAt.Sub(lease.RenewedAt)
	if !lease.RenewedAt.After(heartbeat) || !heartbeat.IsZero() && !now.Before(heartbeat) && now.Sub(heartbeat) < ttl {
		return
	}
	r.Problems = append(r.Problems, runnerauth.Problem{Code: "runtime_expired", ProjectID: string(lease.Lease.ProjectID), Subject: lease.RuntimeAttemptID,
		Message: "A running attempt has expired runtime activity while its lease is still being renewed.",
		FixHint: "Inspect the attempt's runtime and finalization diagnostics on its runner.", FirstSeen: heartbeat.Add(ttl), ReportedAt: lease.RenewedAt})
	if r.Health != "revoked" && r.Health != "expired" {
		r.Health = "needs_attention"
	}
}
