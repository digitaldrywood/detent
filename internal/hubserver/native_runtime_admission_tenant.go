package hubserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runtimeAdmissionPageKey struct{}

type runtimeAdmissionPage struct {
	capacity     *runnerstore.RuntimeCapacitySnapshot
	observations map[string]tracker.NativeAdmissionObservation
}

func (s *Service) readRuntimeAdmissionTenant(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, evidence *tracker.NativeRuntimeEvidence, supplied []tracker.NativeAdmissionContext) error {
	now := s.config.Clock().UTC()
	var records []runnerstore.RuntimeCapacityRunner
	var capacity runnerstore.RuntimeCapacitySnapshot
	var currentLease *tracker.NativeLease
	var observations map[string]tracker.NativeAdmissionObservation
	var revision policystore.Revision
	var candidates claimstore.CandidatePage
	var sourceCandidate claimstore.Candidate
	var unresolved []tracker.NativeDependency
	dependencyPartial := false
	plan := hubrecords.NativeClaimPlan{Issue: issue}
	var answeredPlan hubrecords.NativeClaimPlan
	truncated := false
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		fleet, ok := r.(runnerstore.RuntimeCapacityReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		admission, ok := r.(runnerstore.AdmissionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		revision, err = r.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		shared, _ := ctx.Value(runtimeAdmissionPageKey{}).(*runtimeAdmissionPage)
		if shared != nil && shared.capacity != nil {
			capacity = *shared.capacity
		} else {
			capacity, err = fleet.RuntimeCapacitySnapshot(ctx, runnerstore.RuntimeCapacityQuery{ProjectID: scope.Project, RunnerID: scope.Credential.Runner.RunnerID, At: now, Explanation: hubcontent.ExplanationRead(ctx)})
			if err != nil {
				return err
			}
			if shared != nil {
				selected := capacity
				shared.capacity = &selected
			}
		}
		records, truncated = capacity.Records, capacity.Truncated
		if evidence.CurrentLease == nil {
			item, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
			if err != nil {
				return err
			}
			lease, err := r.ActiveLeaseForIssue(ctx, item.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt) {
				binding, err := r.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
				if err != nil {
					return err
				}
				currentLease = &tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt, PolicyID: binding.PolicyID, ServerTime: now}
			}
		}
		ids := make([]string, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.Runner.RunnerID)
		}
		if shared != nil && shared.observations != nil {
			observations = shared.observations
		} else {
			observations, err = admission.RunnerAdmissionObservations(ctx, scope.Project, ids)
			if err != nil {
				return err
			}
			if shared != nil {
				shared.observations = observations
			}
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		plan.Project = project
		if issue.IssueContract != nil && issue.IssueContract.ReleaseDelivery != nil {
			item, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
			if err != nil {
				return err
			}
			if item.Revision != int64(issue.Revision) {
				return corestore.ErrRevisionConflict
			}
			plan.ReleaseDelivery, err = readReleaseDeliveryContractFacts(ctx, r, scope, issue.WorkItemID, project.States)
			if err != nil {
				return err
			}
		}
		var states []string
		for _, state := range project.States {
			if state.Dispatchable {
				states = append(states, state.Name)
			}
		}
		candidates, err = r.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: scope.Project, Scope: string(scope.Project), WorkItemID: issue.WorkItemID, States: states, AvailableAt: now, Limit: 1})
		if err != nil {
			return err
		}
		if len(candidates.Items) > 0 {
			batch, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			rows, err := batch.ClaimPreviewPlanRows(ctx, scope.Project, candidates.Items[:1])
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				return corestore.ErrRevisionConflict
			}
			if rows[0].Err != nil {
				return rows[0].Err
			}
			answeredPlan = nativeClaimPlanFromBatch(hubrecords.NativeClaimPlan{Project: project}, candidates.Items[0], rows[0])
		}
		if project.Core.Profile == "github_compatible" {
			item, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
			if err != nil {
				return err
			}
			if item.Revision != int64(issue.Revision) {
				return corestore.ErrRevisionConflict
			}
			facts, err := r.WorkItemFacts(ctx, scope.Project, item.NativeID)
			if err != nil {
				return err
			}
			sourceCandidate = claimstore.Candidate{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.NativeID, Revision: item.Revision, Repository: facts.Repository}
		}
		if len(candidates.Items) == 0 && project.Core.RequireDependencies {
			unresolved, dependencyPartial, err = s.readRuntimeUnresolvedDependencies(ctx, r, scope, issue, project)
		}
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
	workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(scope.Organization)+"/"+string(scope.Project), descriptor)
	if err != nil {
		return err
	}
	plan.Descriptor = descriptor
	var answered *tracker.NativeAnsweredExclusion
	if answeredPlan.SnapshotLoaded {
		answered, err = s.nativeClaimAnsweredExclusion(ctx, scope, answeredPlan, now)
		if err != nil {
			return err
		}
	}
	contract, err := nativeRuntimeIssueContractEvaluation(plan)
	if err != nil {
		return err
	}
	if plan.Project.Core.Profile == "github_compatible" && !nativeCompatibleClaimSource(plan.Project, issue, sourceCandidate) {
		contract = nativeContractEvaluation{ReasonCode: "no_claimable_work", Reason: "Current issue source must match the enabled project repository"}
	}
	evidence.Unavailable = slices.DeleteFunc(evidence.Unavailable, func(value string) bool {
		return value == "native_admission_projection" || value == "runner_capacity_projection"
	})
	if evidence.LatestDecision == nil && !slices.Contains(evidence.Unavailable, "historical_scheduler_decision") {
		evidence.Unavailable = append(evidence.Unavailable, "historical_scheduler_decision")
	}
	if truncated {
		evidence.Unavailable = append(evidence.Unavailable, "capacity_projection_truncated")
	}
	if currentLease == nil && evidence.Scheduling.Outcome == "unknown" {
		evidence.Scheduling.Reason = "Current runner admission has not established a dispatch decision"
	}
	if currentLease != nil {
		evidence.CurrentLease = currentLease
		evidence.Scheduling.Outcome = "claimed"
		evidence.Scheduling.Reason = "A current fenced lease owns this item"
	}
	nonExecutable := connector.NonExecutableReason(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels})
	if nonExecutable == "" {
		nonExecutable = nativeRuntimeStateExclusion(plan.Project, issue.State)
	}
	if evidence.CurrentLease == nil && !contract.Allowed && nonExecutable == "" {
		evidence.Scheduling.Outcome, evidence.Scheduling.Reason = "skipped", contract.Reason
	}
	if evidence.CurrentLease == nil && nonExecutable != "" {
		evidence.Scheduling.Outcome, evidence.Scheduling.Reason = "skipped", nonExecutable
	}
	landingReady, landingLimit := runtimeLandingCapacity(capacity.Providers, scope.Project)
	if !hubcontent.ExplanationRead(ctx) && capacity.Truncated {
		landingReady, landingLimit = false, nativeLandingCapacityUnknown
	}
	landingRequired := hubapi.StringsEqualFold(issue.State, "Merging") || issue.ListChange != nil && issue.ListChange.LandingPending
	if landingRequired && !landingReady {
		if landingLimit == nativeLandingCapacityUnknown {
			evidence.Unavailable = append(evidence.Unavailable, "project_landing_capacity")
		}
	}
	runners := runtimeRunnersFromSnapshot(capacity, scope, now)
	for index, record := range records {
		runner := runners[index]
		view := tracker.NativeRuntimeCapacity{ProviderCapacity: runner.ProviderCapacity, RunnerID: runner.RunnerID, ObservedAt: runner.LastHeartbeatAt, Health: runner.Health, Available: max(0, min(runner.HostCapacity-runner.HostUsed, runner.RemainingCapacity(scope.Project))), Exclusions: []string{}}
		for _, exclusion := range runner.Exclusions(scope.Project, descriptor.Requirements, false) {
			view.Exclusions = append(view.Exclusions, exclusion.Code)
		}
		if landingRequired && !landingReady {
			code := "landing_capacity"
			if landingLimit == nativeLandingCapacityUnknown {
				code = "landing_capacity_unknown"
			}
			view.Exclusions = append(view.Exclusions, code)
		}
		evidence.Capacity = append(evidence.Capacity, view)
		entry := tracker.NativeRuntimeAdmission{RunnerID: runner.RunnerID, RunnerRevision: runner.Revision, PolicyID: descriptor.ID, Source: "native_claim_candidate_snapshot", ObservedAt: now, Outcome: "unknown", Reason: "Current runner selectors are unavailable", Unavailable: []string{}}
		observation, found := observations[runner.RunnerID]
		selected := observation.Context
		if len(supplied) == 1 && scope.Credential.Runner.RunnerID == runner.RunnerID {
			selected, found = supplied[0], true
			entry.SelectorSource = "registered_runner_published_context"
		} else {
			found = found && observation.RunnerRevision == runner.Revision && !now.Before(observation.ReceivedAt) && now.Before(observation.ReceivedAt.Add(runnerauth.HeartbeatTimeout))
			entry.SelectorSource = "registered_runner_heartbeat"
		}
		found = found && selected.Validate() == nil && !now.Before(selected.ObservedAt) && now.Before(selected.ObservedAt.Add(runnerauth.HeartbeatTimeout))
		exclusions := runner.Exclusions(scope.Project, descriptor.Requirements, false)
		var versionRefusal *hubapi.NativeError
		if err := runnerVersionError(minimumRunnerVersion(s.config.Version), record.MachineVersion); err != nil && !errors.As(err, &versionRefusal) {
			return err
		}
		switch {
		case nonExecutable != "":
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "inactive_state", nonExecutable
		case len(exclusions) > 0:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", exclusions[0].Code, exclusions[0].Message
		case versionRefusal != nil:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", versionRefusal.Code, versionRefusal.Message
		case truncated:
			entry.Unavailable = append(entry.Unavailable, "capacity_projection_truncated")
		case !contract.Allowed:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", contract.ReasonCode, contract.Reason
		case landingRequired && !nativeChangeLandingClaimable("Merging", evidence.Change) && slices.Contains(evidence.Unavailable, "change_gate_history_truncated"):
			entry.Unavailable = append(entry.Unavailable, "current_change_landing_readiness")
		case landingRequired && !nativeChangeLandingClaimable("Merging", evidence.Change):
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "no_claimable_work", "Current Change version is not ready for native landing"
		case landingRequired && !landingReady && landingLimit != nativeLandingCapacityUnknown:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "landing_capacity", "Project landing capacity is full"
		case len(candidates.Items) == 0:
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "no_claimable_work", "Current native candidate facts exclude this item"
			entry.UnresolvedDependencies = unresolved
			if len(unresolved) > 0 {
				entry.Unavailable = append(entry.Unavailable, "other_native_candidate_exclusions")
			} else {
				entry.Unavailable = append(entry.Unavailable, "native_candidate_exclusion")
			}
			if dependencyPartial {
				entry.Unavailable = append(entry.Unavailable, "unresolved_dependencies")
			}
		case answered != nil:
			entry.Outcome, entry.ReasonCode, entry.Reason, entry.AnsweredExclusion = "skipped", "no_claimable_work", nativeAnsweredExclusionReason(*answered), answered
			entry.Unavailable = append(entry.Unavailable, "other_native_candidate_exclusions")
		case !found:
			entry.Unavailable = append(entry.Unavailable, "runner_specific_candidate_selection")
		case !runtimeAdmissionSelects(selected, candidates.Items[0]):
			entry.SelectorObservedAt = &selected.ObservedAt
			entry.Outcome, entry.ReasonCode, entry.Reason = "skipped", "no_claimable_work", "Current claim selectors do not select this item"
		case workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered:
			entry.SelectorObservedAt = &selected.ObservedAt
			entry.Unavailable = append(entry.Unavailable, "monthly_budget_admission")
		case landingRequired && !landingReady:
			entry.SelectorObservedAt = &selected.ObservedAt
			entry.Unavailable = append(entry.Unavailable, "project_landing_capacity")
		case len(runner.ProviderCapacity) > 0:
			entry.SelectorObservedAt = &selected.ObservedAt
			entry.Unavailable = append(entry.Unavailable, "provider_candidate_requirement")
		default:
			entry.SelectorObservedAt = &selected.ObservedAt
			entry.Outcome, entry.Reason = "ready", "Current runner admission facts select this item"
		}
		evidence.Admission = append(evidence.Admission, entry)
	}
	if evidence.CurrentLease == nil && evidence.Scheduling.Outcome != "skipped" && len(evidence.Admission) > 0 {
		evidence.Scheduling.Source = "native_claim_candidate_snapshot"
		switch {
		case slices.ContainsFunc(evidence.Admission, func(a tracker.NativeRuntimeAdmission) bool { return a.Outcome == "ready" }):
			evidence.Scheduling.Outcome, evidence.Scheduling.Reason = "ready", "A current registered runner permits this native candidate; this snapshot is not a lease grant or dispatch decision"
		case !truncated && !slices.ContainsFunc(evidence.Admission, func(a tracker.NativeRuntimeAdmission) bool { return a.Outcome == "unknown" }):
			evidence.Scheduling.Outcome, evidence.Scheduling.Reason = "skipped", "Current enrolled runners refuse this native candidate"
		}
	}
	return nil
}

func runtimeAdmissionSelects(selected tracker.NativeAdmissionContext, item claimstore.Candidate) bool {
	contains := func(values []string, value string) bool {
		return slices.ContainsFunc(values, func(candidate string) bool { return hubapi.StringsEqualFold(candidate, value) })
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

func nativeRuntimeIssueContractEvaluation(plan hubrecords.NativeClaimPlan) (nativeContractEvaluation, error) {
	allowed := nativeContractEvaluation{Allowed: true}
	if strings.Contains(strings.ToLower(plan.Issue.Body), "detent:no-dispatch") {
		return nativeContractEvaluation{ReasonCode: "no_claimable_work", Reason: "The issue contains detent:no-dispatch; remove the marker to permit dispatch"}, nil
	}
	if plan.Issue.LinkedSource != nil && plan.Issue.LinkedSource.Status != "complete" || strings.EqualFold(plan.Issue.State, "Triage") || plan.Issue.IssueContract != nil && plan.Issue.IssueContract.Exempt {
		return allowed, nil
	}
	contract, err := config.ResolvePolicyIssueContract(plan.Descriptor)
	if err != nil {
		return nativeContractEvaluation{}, err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, plan.Descriptor)
	if err != nil {
		return nativeContractEvaluation{}, err
	}
	target := workflow.Config.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(plan.Issue.State, target) {
		return allowed, nil
	}
	if plan.Issue.IssueContract != nil && plan.Issue.IssueContract.ReleaseDelivery != nil && !releaseDeliveryContractFactsCurrent(plan, contract) {
		return nativeContractEvaluation{ReasonCode: "issue_contract_unsatisfied", Reason: "The verified release delivery contract changed; current approved policy and exact associated delivery are required"}, nil
	}
	evaluation := contract.Evaluate(hubrecords.NativeContractIssue(plan.Issue))
	if evaluation.Satisfied() {
		return allowed, nil
	}
	var reasons []string
	if len(evaluation.Missing) > 0 {
		reasons = append(reasons, "Add or complete these required issue sections: "+strings.Join(evaluation.Missing, ", "))
	}
	if evaluation.NeedsConfirmation {
		reasons = append(reasons, "A human must confirm the success criteria by editing or saving the issue body")
	}
	return nativeContractEvaluation{ReasonCode: "issue_contract_unsatisfied", Reason: strings.Join(reasons, "; "), Missing: evaluation.Missing}, nil
}

func (s *Service) readRuntimeUnresolvedDependencies(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, issue tracker.NativeIssue, project projectstore.Record) ([]tracker.NativeDependency, bool, error) {
	const limit = 100
	anchor, err := r.WorkItem(ctx, scope.Project, issue.WorkItemID)
	if err != nil {
		return nil, false, err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(anchor.ActorPrincipalID)}) {
		return nil, false, corestore.ErrNotFound
	}
	if anchor.Revision != int64(issue.Revision) {
		return nil, false, corestore.ErrRevisionConflict
	}
	rows, err := r.WorkItemBlockers(ctx, scope.Project, issue.WorkItemID, corestore.WorkItemBlockerCursor{}, limit)
	if err != nil {
		return nil, false, err
	}
	type targetProject struct {
		record  projectstore.Record
		visible bool
	}
	projects := map[tracker.ProjectID]targetProject{scope.Project: {record: project, visible: true}}
	var unresolved []tracker.NativeDependency
	organizationSlug := ""
	if s.config.Hosted != nil && len(rows) > 0 {
		organization, err := r.AccessOrganization(ctx)
		if err != nil {
			return nil, false, err
		}
		organizationSlug = organization.Slug
	}
	for _, edge := range rows {
		target, found := projects[edge.ProjectID]
		if !found {
			targetScope := scope
			targetScope.Project = edge.ProjectID
			err := s.authorizeAccessScope(ctx, r, targetScope, false)
			if err != nil && !errors.Is(err, accessstore.ErrDenied) && !errors.Is(err, auth.ErrHostedIdentity) && !errors.Is(err, operatortool.ErrAccessDenied) && !hubapi.IsNativeNotFound(err) && !errors.Is(err, corestore.ErrNotFound) {
				return nil, false, err
			}
			if err == nil {
				target.record, err = r.ProjectRecord(ctx, edge.ProjectID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return nil, false, err
				}
				target.visible = err == nil && target.record.Core.DeletedAt == nil && (target.record.Core.Profile == "native" || target.record.Core.Profile == "github_compatible")
			}
			projects[edge.ProjectID] = target
		}
		if !target.visible {
			continue
		}
		item, err := r.WorkItemByID(ctx, edge.ProjectID, edge.IssueID)
		if err != nil {
			return nil, false, err
		}
		if item.NativeID != edge.WorkItemID {
			return nil, false, corestore.ErrRevisionConflict
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			continue
		}
		stateName := hubapi.StringValue(item.WorkflowStateName)
		var state tracker.NativeState
		for _, candidate := range target.record.States {
			if candidate.Name == stateName {
				state = candidate
				break
			}
		}
		if state.Terminal {
			continue
		}
		dependency := tracker.NativeDependency{ID: item.NativeID, ProjectID: item.ProjectID, Number: int(item.Number), Title: item.Title, Identifier: string(item.ProjectID) + "#" + strconv.FormatInt(item.Number, 10), State: stateName, Dispatchable: state.Dispatchable}
		if organizationSlug != "" && target.record.Core.Slug != "" {
			dependency.WebURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(organizationSlug, target.record.Core.Slug, int(item.Number))}).WebURL
		}
		unresolved = append(unresolved, dependency)
	}
	slices.SortFunc(unresolved, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return unresolved[:min(limit, len(unresolved))], len(rows) > limit, nil
}

func nativeAnsweredExclusionReason(answered tracker.NativeAnsweredExclusion) string {
	if answered.Kind == string(gate.ReasonAutomatedReviewMissing) {
		return "The existing native claim candidate query does not select this item; waiting on review evidence for the published current Change version"
	}
	return "The existing native claim candidate query does not select this item; current answered exclusion: " + answered.Kind
}

func nativeRuntimeStateExclusion(project projectstore.Record, state string) string {
	for _, candidate := range project.States {
		if !hubapi.StringsEqualFold(candidate.Name, state) {
			continue
		}
		if candidate.Terminal {
			return fmt.Sprintf("Native workflow state %s is terminal", state)
		}
		if !candidate.Dispatchable {
			return fmt.Sprintf("Native workflow state %s is not dispatchable", state)
		}
	}
	return ""
}
