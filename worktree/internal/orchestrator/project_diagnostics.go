package orchestrator

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
)

func (s State) ProjectDiagnostics(active []store.WorkAttempt, observed time.Time) *runnerauth.ProjectDiagnostics {
	d := &runnerauth.ProjectDiagnostics{ObservedAt: observed, RuntimeObservedAt: s.RuntimeObservation.ObservedAt, Source: "runner_runtime_and_durable_attempt_owners", Records: []runnerauth.DiagnosticAttempt{}, Admissions: []runnerauth.DiagnosticAdmission{}, Counts: map[string]int{"running": len(s.Running), "claimed": len(s.Claimed), "deferred": len(s.deferredCompletions), "durable_active": len(active), "runtime_unsettled": s.UnsettledWork()}, Unavailable: map[string]string{"running_build": "not_reported_by_local_owner"}}
	records := map[string]runnerauth.DiagnosticAttempt{}
	add := func(r runnerauth.DiagnosticAttempt, membership string) {
		r.Key = runnerauth.DiagnosticKey(r.LocalAttemptID, r.NativeAttemptID, r.IssueID)
		if previous, ok := records[r.Key]; ok {
			r.Membership = append(previous.Membership, membership)
			if r.Generation == 0 {
				r.Generation = previous.Generation
			}
			if r.Stage == "" {
				r.Stage = previous.Stage
			}
			if r.HostOperation == "" {
				r.HostOperation, r.OperationPending, r.ProviderCompletedAt, r.DeferredAt, r.RetryAt, r.RetryAttempt, r.LatestError, r.RecoveryOwner = previous.HostOperation, previous.OperationPending, previous.ProviderCompletedAt, previous.DeferredAt, previous.RetryAt, previous.RetryAttempt, previous.LatestError, previous.RecoveryOwner
			}
		} else {
			r.Membership = []string{membership}
		}
		if r.Unavailable == nil {
			r.Unavailable = map[string]string{}
		}
		if r.NativeAttemptID == "" {
			r.Unavailable["native_attempt_id"] = "unrecorded"
		}
		if r.Fence == 0 {
			r.Unavailable["fence"] = "unrecorded"
		}
		if r.Generation == 0 {
			r.Unavailable["generation"] = "unrecorded"
		}
		if r.ProviderCompletedAt.IsZero() {
			r.Unavailable["provider_completion"] = "unrecorded"
		}
		if r.HostOperation == "" {
			r.Unavailable["host_operation"] = "unrecorded"
		}
		records[r.Key] = r
	}
	deferred := func(r deferredCompletion) runnerauth.DiagnosticAttempt {
		return runnerauth.DiagnosticAttempt{IssueID: r.Running.Issue.ID, LocalAttemptID: r.Running.WorkAttemptID, Generation: r.Running.Generation, Stage: r.Running.Mode, ProviderCompletedAt: r.CompletedAt, HostOperation: "orchestrator.retry_deferred_completion", OperationPending: true, DeferredAt: r.DeferredAt, RetryAt: r.FenceRetryAt, RetryAttempt: r.RetryAttempt, LatestError: runnerauth.DiagnosticError(r.Error), RecoveryOwner: "orchestrator.deferred_completion", Unavailable: map[string]string{"inner_completion_operation": "unrecorded"}}
	}
	for _, r := range s.Running {
		add(runnerauth.DiagnosticAttempt{IssueID: r.Issue.ID, LocalAttemptID: r.WorkAttemptID, Generation: r.Generation, Stage: r.Mode}, "running")
	}
	for _, r := range s.deferredCompletions {
		add(deferred(r), "deferred")
	}
	for _, a := range active {
		r := runnerauth.DiagnosticAttempt{IssueID: a.IssueID, LocalAttemptID: a.ID, Stage: a.Phase, LeaseRenewedAt: a.HeartbeatAt, LeaseExpiresAt: a.LeaseExpiresAt, LatestError: runnerauth.DiagnosticError(a.ErrorMessage)}
		if saved, err := decodeDeferredCompletion(a); err == nil {
			r = deferred(saved)
			r.LeaseRenewedAt, r.LeaseExpiresAt = a.HeartbeatAt, a.LeaseExpiresAt
		}
		add(r, "durable_active")
	}
	for id, c := range s.Claimed {
		matched := false
		for key, r := range records {
			if r.IssueID == id && (slices.Contains(r.Membership, "running") || slices.Contains(r.Membership, "deferred")) {
				r.Membership = append(r.Membership, "claimed")
				r.LeaseRenewedAt, r.LeaseExpiresAt = c.LeaseRenewedAt, c.LeaseExpiresAt
				records[key] = r
				matched = true
			}
		}
		if !matched {
			add(runnerauth.DiagnosticAttempt{IssueID: id, LeaseRenewedAt: c.LeaseRenewedAt, LeaseExpiresAt: c.LeaseExpiresAt, Unavailable: map[string]string{"local_attempt_id": "claim_has_no_attempt_identity"}}, "claimed")
		}
	}
	for _, r := range records {
		d.Records = append(d.Records, r)
	}
	d.Counts["distinct_records"] = len(d.Records)
	seen := map[string]bool{}
	for _, a := range s.SchedulerDecisions {
		if a.IssueID == "" || a.DecisionAt.IsZero() || a.Reason == "" || seen[a.IssueID] {
			continue
		}
		seen[a.IssueID] = true
		admission := runnerauth.DiagnosticAdmission{IssueID: a.IssueID, ObservedAt: a.DecisionAt, Result: a.Result, Predicate: a.Reason, Unavailable: map[string]string{"provider_requirement": "unrecorded", "landing_reservations": "unrecorded"}}
		var metadata struct {
			Admission *runnerauth.DiagnosticAdmission `json:"runner_admission"`
		}
		if json.Unmarshal([]byte(a.RunnerAdmissionJSON), &metadata) == nil && metadata.Admission != nil {
			admission = *metadata.Admission
		}
		d.Admissions = append(d.Admissions, admission)
	}
	d.Bound()
	return d
}

func (o *Orchestrator) recordLocalAdmission(ctx context.Context, state *State, issue connector.Issue, now time.Time, allowed bool, reason, predicate string, requirement *providercapacity.Requirement) {
	result := "skipped"
	if allowed {
		result = "eligible"
	}
	if predicate == "" {
		predicate = "local_candidate_ready"
	}
	observation := runnerauth.DiagnosticAdmission{IssueID: issue.ID, ObservedAt: now, Result: result, Predicate: predicate, ProviderRequirement: requirement, Unavailable: map[string]string{}, RepositoryReservations: []string{}}
	if requirement == nil {
		observation.Unavailable["provider_requirement"] = "not_resolved_at_this_predicate"
	}
	repository := mergeWorkerRepositoryKey(issue)
	for id, reservation := range state.mergeReservations {
		if repository != "" && reservation.Repository == repository {
			observation.RepositoryReservations = append(observation.RepositoryReservations, id)
		}
	}
	for _, running := range state.Running {
		if mergeWorkerIssue(running.Issue) {
			observation.HostLandingOccupancy++
		}
	}
	observation.Unavailable["other_project_host_reservations"] = "not_in_project_owner"
	record := o.schedulerDecisionRecord(state, now, dispatchPlanDecision{Issue: issue}, result, reason)
	record.MetadataJSON = marshalWorkAttemptJSON(map[string]any{"runner_admission": observation})
	o.recordSchedulerDecisions(ctx, state, []store.SchedulerDecision{record}, false)
}
