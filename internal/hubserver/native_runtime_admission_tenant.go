package hubserver

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/config"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readRuntimeAdmissionTenant(ctx context.Context, scope nativeScope, issue tracker.NativeIssue, evidence *tracker.NativeRuntimeEvidence, supplied []tracker.NativeAdmissionContext) error {
	const limit = 100
	now := s.config.now().UTC()
	var records []runnerstore.FleetRecord
	var leases []runnerstore.FleetLease
	var observations map[string]tracker.NativeAdmissionObservation
	var revision policystore.Revision
	var candidates claimstore.CandidatePage
	var urgent runnerstore.UrgentUpdate
	truncated := false
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		fleet, ok := r.(runnerstore.FleetReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		admission, ok := r.(runnerstore.AdmissionReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		revision, err = r.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		records, err = fleet.FleetRecords(ctx, limit+1)
		if err != nil {
			return err
		}
		truncated = len(records) > limit
		if truncated {
			records = records[:limit]
		}
		leases, err = fleet.FleetLeases(ctx, now, limit+1)
		if err != nil {
			return err
		}
		truncated = truncated || len(leases) > limit
		ids := make([]string, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.Identity.RunnerID)
		}
		observations, err = admission.RunnerAdmissionObservations(ctx, scope.project, ids)
		if err != nil {
			return err
		}
		urgent, err = r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		var states []string
		for _, state := range project.States {
			if state.Dispatchable {
				states = append(states, state.Name)
			}
		}
		candidates, err = r.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: scope.project, Scope: string(scope.project), WorkItemID: issue.WorkItemID, States: states, AvailableAt: now, Limit: 1})
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		evidence.Unavailable = append(evidence.Unavailable, "approved_dispatch_policy")
		return nil
	}
	if err != nil {
		return err
	}
	descriptor, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, revision)
	if err != nil {
		return err
	}
	workflow, err := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), descriptor)
	if err != nil {
		return err
	}
	evidence.Unavailable = slices.DeleteFunc(evidence.Unavailable, func(value string) bool { return value == "native_admission_projection" })
	providers := tenantFleetProviderViews(records, leases, now)
	for _, record := range records {
		runner := runnerFromFleetRecord(record, now, urgent)
		runner.ProjectUsed = make(map[tracker.ProjectID]int)
		for _, lease := range leases {
			if lease.MachineID == runner.MachineID {
				runner.HostUsed++
			}
			if lease.RunnerID == runner.RunnerID {
				runner.Used++
				runner.ProjectUsed[lease.Lease.ProjectID]++
			}
		}
		runner.ProviderCapacity = providers[runner.RunnerID]
		entry := tracker.NativeRuntimeAdmission{RunnerID: runner.RunnerID, RunnerRevision: runner.Revision, PolicyID: descriptor.ID, Source: "native_claim_candidate_snapshot", ObservedAt: now, Outcome: "unknown", Reason: "Current runner selectors are unavailable", Unavailable: []string{}}
		observation, found := observations[runner.RunnerID]
		selected := observation.Context
		if len(supplied) == 1 && scope.credential.Runner.RunnerID == runner.RunnerID {
			selected, found = supplied[0], true
			entry.SelectorSource = "registered_runner_published_context"
		} else {
			found = found && observation.RunnerRevision == runner.Revision && !now.Before(observation.ReceivedAt) && now.Before(observation.ReceivedAt.Add(runnerauth.HeartbeatTimeout))
			entry.SelectorSource = "registered_runner_heartbeat"
		}
		found = found && selected.Validate() == nil && selected.PolicyID == descriptor.ID && !now.Before(selected.ObservedAt) && now.Before(selected.ObservedAt.Add(runnerauth.HeartbeatTimeout))
		if found {
			entry.SelectorObservedAt = &selected.ObservedAt
		}
		exclusions := runner.Exclusions(scope.project, descriptor.Requirements, false)
		switch {
		case len(exclusions) > 0:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", exclusions[0].Code, exclusions[0].Message
		case runnerVersionError(minimumRunnerVersion(s.config.Version), record.Machine.Version) != nil:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "runner_version_unsupported", "Runner update is required"
		case truncated:
			entry.Unavailable = append(entry.Unavailable, "capacity_projection_truncated")
		case !nativeClaimIssueContractAllowed(nativeClaimPlan{issue: issue, descriptor: descriptor}):
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "issue_contract", "The current issue contract does not permit dispatch"
		case !found:
			entry.Unavailable = append(entry.Unavailable, "runner_specific_candidate_selection")
		case len(candidates.Items) == 0 || !runtimeAdmissionSelects(selected, candidates.Items[0]):
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "no_claimable_work", "Current claim selectors do not select this item"
		case workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered:
			entry.Unavailable = append(entry.Unavailable, "monthly_budget_admission")
		case stringsEqualFold(issue.State, "Merging"):
			entry.Unavailable = append(entry.Unavailable, "current_change_landing_readiness")
		default:
			entry.Outcome, entry.Reason = "ready", "Current runner admission facts select this item"
		}
		evidence.Admission = append(evidence.Admission, entry)
	}
	return nil
}

func runtimeAdmissionSelects(selected tracker.NativeAdmissionContext, item claimstore.Candidate) bool {
	contains := func(values []string, value string) bool {
		return slices.ContainsFunc(values, func(candidate string) bool { return stringsEqualFold(candidate, value) })
	}
	if !contains(selected.WorkflowStates, item.State) || len(selected.Authors) > 0 && !contains(selected.Authors, item.Author) {
		return false
	}
	if len(selected.Assignees) > 0 && !slices.ContainsFunc(item.Assignees, func(value string) bool { return contains(selected.Assignees, value) }) {
		return false
	}
	for _, label := range selected.LabelInclude {
		if !contains(item.Labels, label) {
			return false
		}
	}
	return !slices.ContainsFunc(item.Labels, func(value string) bool { return contains(selected.LabelExclude, value) })
}
