package runnerfleet

import (
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/sprites"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const cachedProbeProblemWindow = 2 * time.Minute

func hubProblems(protocol int, rejected bool) []runnerauth.Problem {
	problems := []runnerauth.Problem{}
	if rejected {
		problems = append(problems, runnerauth.NewProblem("settings_rejected"))
	}
	if protocol != 0 && protocol != 2 {
		problems = append(problems, runnerauth.NewProblem("version_unsupported"))
	}
	return problems
}

func ApplyProblemView(r *runnerauth.Runner, spriteName string, now time.Time) {
	r.Problems = runnerauth.MergeProblems(r.Problems, r.Problems, r.LastHeartbeatAt)
	if spriteName != "" && r.IsolationTier == isolation.Sandbox {
		for index := range r.Problems {
			if r.Problems[index].Code == "tier_unavailable" {
				r.Problems[index].Message = sprites.SandboxUnavailableReason
			}
		}
	}
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
}

func MergeProblems(previous, current []runnerauth.Problem, protocol int, rejected bool, now time.Time) []runnerauth.Problem {
	current = slices.DeleteFunc(slices.Clone(current), func(p runnerauth.Problem) bool {
		return p.Code == "settings_rejected" || p.Code == "version_unsupported" || p.Code == "policy_mismatch"
	})
	current = append(current, hubProblems(protocol, rejected)...)
	return runnerauth.MergeProblems(previous, current, now)
}

func AppendRuntimeProblems(r *runnerauth.Runner, lease runnerstore.FleetLease, now time.Time) {
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
