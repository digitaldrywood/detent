package hubserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimsource"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const nativeClaimCandidatePageSize = 100

var errNativeClaimPlanRefused = fmt.Errorf("claim plan refused: %w", ErrNoClaimableWork)

type nativeClaimCandidateScan struct {
	plans    []hubrecords.NativeClaimPlan
	replay   *hubrecords.NativeClaimPlan
	pageNext claimstore.CandidateCursor
	cursor   claimstore.SelectionCursor
}

var errClaimHostSlotTaken = fmt.Errorf("runner host slot taken: %w", ErrNoClaimableWork)

func (s *Service) claimNativeIssueTenant(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, ttl time.Duration) (leasestore.Lease, error) {
	if s.config.TenantRepository == nil || s.config.NativeRunEvents == nil || scope.Credential.Runner.RunnerID == "" {
		return leasestore.Lease{}, hubapi.NativeRunEventStorageUnavailable()
	}
	explicitItem := request.WorkItemID != ""
	if request.WorkItemID == "" && len(request.ProviderCandidates) == 1 {
		request.WorkItemID = request.ProviderCandidates[0].WorkItemID
	}
	if request.WorkItemID != "" && len(request.ProviderCandidates) > 0 && !slices.ContainsFunc(request.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool {
		return candidate.WorkItemID == request.WorkItemID
	}) {
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	if s.runDetailStore() == nil {
		return leasestore.Lease{}, hubapi.NativeRunEventStorageUnavailable()
	}
	now := s.config.Clock().UTC()
	if request.WorkItemID == "" {
		scan, err := s.readNativeClaimCandidates(ctx, scope, request, now)
		if err != nil {
			return leasestore.Lease{}, err
		}
		if scan.replay != nil {
			plan, err := s.hydrateNativeClaimPlan(ctx, scope, request, now, *scan.replay, nil)
			if err != nil {
				return leasestore.Lease{}, err
			}
			return s.claimNativeIssueTenantWithPlan(ctx, scope, request, ttl, now, plan, nil)
		}
		position := scan.cursor.Position
		var descriptor *policy.Descriptor
		var budgetRefusal, providerRefusal error
		placement := &hubrecords.ClaimPlacementSnapshot{}
		sourceBudget := claimSourceBudget{remaining: claimSourceClassificationLimit}
		sourcePending := false
		for _, rows := range scan.plans {
			candidate := rows.Candidate
			if candidate.SourceUnclassified {
				state, err := s.classifyClaimCandidateSource(ctx, scope, candidate, &sourceBudget)
				if err != nil {
					return leasestore.Lease{}, err
				}
				if state == claimSourcePending {
					sourcePending = true
					break
				}
				if state == claimSourceUnverifiable {
					position = claimCandidateCursor(candidate)
					continue
				}
			}
			rows.Placement = placement
			plan, err := s.hydrateNativeClaimPlan(ctx, scope, request, now, rows, descriptor)
			if errors.Is(err, ErrNoClaimableWork) {
				position = claimCandidateCursor(candidate)
				continue
			}
			if err != nil {
				return leasestore.Lease{}, err
			}
			descriptor = &plan.Descriptor
			if !nativeClaimIssueContractAllowed(plan) {
				s.config.Logger.InfoContext(ctx, "native claim candidate skipped",
					"organization_id", scope.Organization, "project_id", scope.Project,
					"work_item_id", candidate.WorkItemID, "work_item_revision", plan.Item.Revision,
					"runner_id", scope.Credential.Runner.RunnerID, "reason", "issue_contract_unsatisfied")
				position = claimCandidateCursor(candidate)
				continue
			}
			if (hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending) && !plan.ChangeReady {
				position = claimCandidateCursor(candidate)
				continue
			}
			candidateRequest := request
			candidateRequest.WorkItemID = candidate.WorkItemID
			outranked, err := s.nativeClaimOutranked(ctx, scope, candidateRequest, plan, now)
			if err != nil {
				return leasestore.Lease{}, err
			}
			if outranked {
				return leasestore.Lease{}, ErrNoClaimableWork
			}
			scan.cursor.Position = position
			lease, err := s.claimNativeIssueTenantWithPlan(ctx, scope, candidateRequest, ttl, now, plan, &scan.cursor)
			if errors.Is(err, errClaimHostSlotTaken) {
				return leasestore.Lease{}, ErrNoClaimableWork
			}
			var refusal *hubapi.NativeError
			if errors.As(err, &refusal) && refusal.Code == "provider_capacity" {
				if _, ok := refusal.Details["monthly_budget"].(budget.MonthlyDecision); ok {
					budgetRefusal = err
					position = claimCandidateCursor(candidate)
					continue
				}
			}
			if isProviderWait(err) {
				providerRefusal = err
				position = claimCandidateCursor(candidate)
				continue
			}
			if errors.Is(err, ErrNoClaimableWork) || errors.Is(err, tracker.ErrLeaseConflict) {
				position = claimCandidateCursor(candidate)
				continue
			}
			if recordErr := s.recordClaimRoutingRefusal(ctx, scope, candidateRequest, now, recordableClaimRoutingRefusal(err, plan.Descriptor.Requirements)); recordErr != nil {
				return leasestore.Lease{}, recordErr
			}
			return lease, err
		}
		if len(scan.plans) == 0 && scan.cursor.Position == (claimstore.CandidateCursor{}) && scan.pageNext == (claimstore.CandidateCursor{}) {
			return leasestore.Lease{}, ErrNoClaimableWork
		}
		scan.cursor.Position = scan.pageNext
		if sourcePending {
			scan.cursor.Position = position
		}
		if err := s.persistNativeClaimCursor(ctx, scope, scan.cursor); err != nil {
			return leasestore.Lease{}, err
		}
		if budgetRefusal != nil {
			return leasestore.Lease{}, budgetRefusal
		}
		if providerRefusal != nil {
			return leasestore.Lease{}, providerRefusal
		}
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	plan, err := s.readNativeClaimPlan(ctx, scope, request, now)
	if err != nil {
		if errors.Is(err, ErrNoClaimableWork) && explicitItem {
			if refusal, requirements, selectorErr := s.explicitClaimSelectorRefusal(ctx, scope, request, now); selectorErr != nil {
				return leasestore.Lease{}, selectorErr
			} else if refusal != nil {
				if recordErr := s.recordClaimRoutingRefusal(ctx, scope, request, now, recordableClaimRoutingRefusal(refusal, requirements)); recordErr != nil {
					return leasestore.Lease{}, recordErr
				}
				return leasestore.Lease{}, refusal
			}
			if recordErr := s.recordRequestedClaimRefusal(ctx, scope, request, now, errors.Is(err, errNativeClaimPlanRefused), "no_claimable_work", nil); recordErr != nil {
				return leasestore.Lease{}, recordErr
			}
		}
		return leasestore.Lease{}, err
	}
	contract, err := nativeRuntimeIssueContractEvaluation(plan)
	if err != nil {
		return leasestore.Lease{}, err
	}
	if !contract.Allowed {
		if explicitItem {
			if err := s.recordRequestedClaimRefusal(ctx, scope, request, now, true, contract.Reason, nil); err != nil {
				return leasestore.Lease{}, err
			}
		}
		if contract.ReasonCode == "issue_contract_unsatisfied" {
			return leasestore.Lease{}, &hubapi.NativeError{Code: contract.ReasonCode, Message: contract.Reason, Status: http.StatusConflict, PublicMessage: true}
		}
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	outranked, err := s.nativeClaimOutranked(ctx, scope, request, plan, now)
	if err != nil {
		return leasestore.Lease{}, err
	}
	if outranked {
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	lease, err := s.claimNativeIssueTenantWithPlan(ctx, scope, request, ttl, now, plan, nil)
	if recordErr := s.recordClaimRoutingRefusal(ctx, scope, request, now, recordableClaimRoutingRefusal(err, plan.Descriptor.Requirements)); recordErr != nil {
		return leasestore.Lease{}, recordErr
	}
	return lease, err
}

type requestedClaimRefusal struct {
	item            corestore.WorkItem
	change          tracker.NativeChangeReference
	claimedSequence int64
	eventID         string
	recorded        bool
}

type claimRoutingRefusal struct {
	code, reason string
	requirements policy.Requirements
	sourceFacts  hubrecords.ClaimSourceFacts
}

func (s *Service) explicitClaimSelectorRefusal(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time) (*hubapi.NativeError, policy.Requirements, error) {
	var identity runnerstore.Identity
	var current policystore.Revision
	visible := false
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, policy.Requirements{}, hubapi.NativeRunEventStorageUnavailable()
	}
	if err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		identity, err = r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if err := runnerIdentityClaimRefusal(identity, scope, request.MachineID, now); err != nil {
			return err
		}
		current, err = r.ProjectPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil || current.PolicyID != request.PolicyID {
			return err
		}
		if _, err := r.WorkItem(ctx, scope.Project, request.WorkItemID); errors.Is(err, corestore.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		visible = true
		return nil
	}); err != nil {
		return nil, policy.Requirements{}, err
	}
	if !visible {
		return nil, policy.Requirements{}, nil
	}
	descriptor, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, current)
	if err != nil {
		return nil, policy.Requirements{}, err
	}
	if err := descriptor.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
		return &hubapi.NativeError{Code: "selector_no_match", Message: err.Error(), Status: http.StatusConflict}, descriptor.Requirements, nil
	}
	return nil, policy.Requirements{}, nil
}

func recordableClaimRoutingRefusal(err error, requirements policy.Requirements) *claimRoutingRefusal {
	var refusal *hubapi.NativeError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict {
		return nil
	}
	switch refusal.Code {
	case "selector_no_match", "runner_draining", "host_capacity", "runner_capacity":
		return &claimRoutingRefusal{code: refusal.Code, requirements: requirements}
	}
	return nil
}

func (r claimRoutingRefusal) source() string {
	switch r.code {
	case "selector_no_match":
		return "native_claim_policy"
	case "host_capacity":
		return "native_host_capacity"
	}
	return "native_runner_routing"
}

func (r claimRoutingRefusal) holds(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, request tracker.NativeClaim, identity runnerstore.Identity, now time.Time) (bool, error) {
	identityRefusal := runnerIdentityClaimRefusal(identity, scope, request.MachineID, now)
	switch r.code {
	case "runner_draining":
		var failure *hubapi.NativeError
		return errors.As(identityRefusal, &failure) && failure.Code == "runner_draining", nil
	case "selector_no_match":
		return identityRefusal == nil && r.requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags) != nil, nil
	case "host_capacity":
		if identityRefusal != nil {
			return false, nil
		}
		machine, err := reader.Machine(ctx, string(identity.MachineID))
		if err != nil {
			return false, err
		}
		return machine.Capacity <= 0, nil
	case "runner_capacity":
		if identityRefusal != nil {
			return false, nil
		}
		available, projectCapacity, projectConfigured, err := runnerClaimCapacity(identity, scope.Project, now)
		if err != nil || available <= 0 {
			return err == nil, err
		}
		usage, err := reader.ClaimCapacityUsage(ctx, string(identity.MachineID), identity.RunnerID, scope.Project, "", now)
		return err == nil && (usage.Runner >= int64(available) || projectConfigured && usage.Project >= int64(projectCapacity)), err
	case "source_unavailable":
		item, err := reader.WorkItem(ctx, scope.Project, request.WorkItemID)
		if err != nil || identityRefusal != nil {
			return false, err
		}
		reason, err := claimsource.Refusal(ctx, reader, scope.Project, item, r.sourceFacts, string(identity.MachineID), identity.RunnerID)
		return reason == r.reason, err
	}
	return false, nil
}

func runnerClaimCapacity(identity runnerstore.Identity, project tracker.ProjectID, now time.Time) (int, int, bool, error) {
	projectCapacity, projectConfigured := identity.ProjectCapacityLimits[project]
	if identity.CapacityLimit <= 0 || identity.CapacityConfig == nil {
		return 0, projectCapacity, projectConfigured, nil
	}
	capacity := min(identity.CapacityLimit, identity.ReportedCapacity, int64(identity.CapacityConfig.LocalLimit), int64(identity.CapacityConfig.ClientLimit), int64(identity.CapacityConfig.RuntimeLimit))
	if projectConfigured {
		capacity = min(capacity, int64(projectCapacity))
	}
	availability := runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: identity.Availability.Windows, WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline}
	available, err := availability.Capacity(now, int(capacity))
	return available, projectCapacity, projectConfigured, err
}

func (s *Service) recordClaimRoutingRefusal(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, refusal *claimRoutingRefusal) error {
	if refusal == nil || request.WorkItemID == "" {
		return nil
	}
	err := s.recordRequestedClaimRefusal(ctx, scope, request, now, false, cmp.Or(refusal.reason, refusal.code), refusal)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil
	}
	return err
}

func (s *Service) readRequestedClaimRefusal(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, planRefused bool, reason string, routing *claimRoutingRefusal) (requestedClaimRefusal, error) {
	var result requestedClaimRefusal
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return result, err
	}
	identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
	if err != nil {
		return result, err
	}
	if routing == nil {
		if err := runnerIdentityClaimRefusal(identity, scope, request.MachineID, s.config.Clock().UTC()); err != nil {
			return result, err
		}
	} else if holds, err := routing.holds(ctx, r, scope, request, identity, s.config.Clock().UTC()); err != nil {
		return result, err
	} else if !holds {
		return result, corestore.ErrRevisionConflict
	}
	currentPolicy, err := r.ProjectPolicy(ctx, scope.Project)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, missingClaimPolicyRefusal(ctx, r, scope.Project, request.PolicyID)
	}
	if err != nil {
		return result, err
	}
	if currentPolicy.PolicyID != request.PolicyID {
		return result, claimPolicyRefusal(ctx, r, scope.Project, request.PolicyID)
	}
	result.item, err = r.WorkItem(ctx, scope.Project, request.WorkItemID)
	if err != nil {
		return result, err
	}
	page, err := r.CandidatePage(ctx, nativeTenantCandidateQuery(scope, request, request.WorkItemID, now))
	if err != nil {
		return result, err
	}
	if len(page.Items) != 0 && !planRefused && routing == nil {
		report, err := r.RunnerBackendIsolation(ctx, identity.RunnerID)
		if err != nil {
			return result, err
		}
		claimable := report.SupportsOn(identity.IsolationTier, identity.OS)
		if claimable && slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) {
			claimable, err = s.nativeWorkspaceClaimAllowed(ctx, r, scope, request.WorkItemID, now)
			if err != nil {
				return result, err
			}
		}
		if claimable {
			return result, corestore.ErrRevisionConflict
		}
	}
	change, err := r.LatestChange(ctx, scope.Project, request.WorkItemID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return result, err
	}
	if err == nil {
		result.change.ChangeID = change.ID
		if change.CurrentVersionID != nil {
			version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return result, err
			}
			result.change.VersionID, result.change.HeadSHA = version.ID, version.HeadSHA
		}
	}
	owner, ok := r.(eventstore.SchedulerDecisionReader)
	if !ok {
		return result, hubapi.NativeRunEventStorageUnavailable()
	}
	claimed, err := owner.LatestClaimedSchedulerDecision(ctx, scope.Project, request.WorkItemID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return result, err
	}
	if err == nil {
		result.claimedSequence = claimed.Sequence
	}
	key, err := json.Marshal(struct {
		Organization tracker.OrganizationID
		Project      tracker.ProjectID
		Item         tracker.NativeWorkItemID
		Runner       string
		Revision     int64
		Change       tracker.NativeChangeReference
		Reason       string
		Claimed      int64
	}{scope.Organization, scope.Project, request.WorkItemID, identity.RunnerID, result.item.Revision, result.change, reason, result.claimedSequence})
	if err != nil {
		return result, err
	}
	result.eventID = "evt_refusal_" + policy.Digest(key)
	previous, err := owner.SchedulerDecision(ctx, scope.Project, request.WorkItemID, result.eventID)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	actor := scope.Actor()
	if previous.SchedulerOutcome != "skipped" || previous.ActorKind != actor.Kind || previous.ActorPrincipalID != actor.PrincipalID || previous.EventRevision != result.item.Revision || previous.ChangeID != result.change.ChangeID || previous.VersionID != result.change.VersionID || previous.HeadSHA != result.change.HeadSHA {
		return result, artifact.ErrIntegrity
	}
	result.recorded = true
	return result, nil
}

func (s *Service) recordRequestedClaimRefusal(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, planRefused bool, reason string, routing *claimRoutingRefusal) error {
	var refusal requestedClaimRefusal
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	if err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		refusal, err = s.readRequestedClaimRefusal(ctx, r, scope, request, now, planRefused, reason, routing)
		return err
	}); err != nil || refusal.recorded {
		return err
	}
	source := "native_claim_eligibility"
	if routing != nil {
		source = routing.source()
	}
	decision := tracker.CollaborationEvent{ID: refusal.eventID, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: request.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: tracker.Revision(refusal.item.Revision), Decision: &tracker.NativeSchedulerDecision{RunnerID: scope.Credential.Runner.RunnerID, Source: source, Outcome: "skipped", Reason: reason, At: now, WorkItemRevision: tracker.Revision(refusal.item.Revision)}}}
	if refusal.change.ChangeID != "" {
		decision.Data.Change = &refusal.change
	}
	raw, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	row, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return err
	}
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if _, err := w.LockWorkItem(ctx, scope.Project, request.WorkItemID); err != nil {
			return err
		}
		current, err := s.readRequestedClaimRefusal(ctx, w, scope, request, s.config.Clock().UTC(), planRefused, reason, routing)
		if err != nil {
			return err
		}
		if current.item.Revision != refusal.item.Revision || current.change != refusal.change || current.claimedSequence != refusal.claimedSequence {
			return corestore.ErrRevisionConflict
		}
		if current.recorded {
			return nil
		}
		_, err = w.AppendEvent(ctx, row)
		return err
	})
}

func (s *Service) readNativeClaimCandidates(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time) (nativeClaimCandidateScan, error) {
	var scan nativeClaimCandidateScan
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return scan, hubapi.NativeRunEventStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !nativeClaimProjectEnabled(project) {
			return ErrNoClaimableWork
		}
		policy, err := r.ProjectPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			return missingClaimPolicyRefusal(ctx, r, scope.Project, request.PolicyID)
		}
		if err != nil {
			return err
		}
		if policy.PolicyID != request.PolicyID {
			return claimPolicyRefusal(ctx, r, scope.Project, request.PolicyID)
		}
		ranking, err := readNativeClaimRanking(ctx, r, scope)
		if err != nil {
			return err
		}
		shared := hubrecords.NativeClaimPlan{Project: project, Policy: policy, Ranking: ranking}
		if request.SessionID != "" {
			lease, err := r.LeaseBySession(ctx, request.SessionID)
			if err == nil {
				item, err := r.WorkItemByID(ctx, scope.Project, lease.IssueID)
				if err != nil {
					return err
				}
				candidate := claimstore.Candidate{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: item.ID, Revision: item.Revision, State: hubapi.ValueOrEmpty(item.WorkflowStateName), CreatedAt: item.NativeCreatedAt}
				shared.Replay = &lease
				plan, err := s.readNativeClaimPlanRows(ctx, scope, request, now, r, &candidate, shared)
				if err != nil {
					return err
				}
				scan.replay = &plan
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		query := nativeTenantCandidateQuery(scope, request, "", now)
		sourceRevision, err := nativeClaimSelectionSourceRevision(project, policy, query)
		if err != nil {
			return err
		}
		query.IncludeUnclassifiedSource = true
		cursor, err := r.SelectionCursor(ctx, scope.Credential.Runner.RunnerID, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			cursor = claimstore.SelectionCursor{OrganizationID: scope.Organization, RunnerID: scope.Credential.Runner.RunnerID, ProjectID: scope.Project, SourceRevision: sourceRevision}
		} else if err != nil {
			return err
		} else if cursor.SourceRevision != sourceRevision {
			cursor.SourceRevision = sourceRevision
			cursor.Position = claimstore.CandidateCursor{}
		} else if cursor.Position.WorkItemID != "" {
			anchorQuery := query
			anchorQuery.WorkItemID = cursor.Position.WorkItemID
			anchorQuery.IncludeLeasedAnchor = true
			anchorQuery.Limit = 1
			anchor, err := r.CandidatePage(ctx, anchorQuery)
			if errors.Is(err, corestore.ErrNotFound) || err == nil && (len(anchor.Items) != 1 || claimCandidateCursor(anchor.Items[0]) != cursor.Position) {
				cursor.Position = claimstore.CandidateCursor{}
			} else if err != nil {
				return err
			}
		}
		query.Limit = max(1, nativeClaimCandidatePageSize/max(1, ranking.Reachable))
		query.After = cursor.Position
		page, err := r.CandidatePage(ctx, query)
		if err != nil {
			return err
		}
		scan = nativeClaimCandidateScan{pageNext: page.Next, cursor: cursor}
		if len(page.Items) == 0 {
			return nil
		}
		batchReader, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		rows, err := batchReader.ClaimPreviewPlanRows(ctx, scope.Project, page.Items)
		if err != nil {
			return err
		}
		if len(rows) != len(page.Items) {
			return corestore.ErrRevisionConflict
		}
		for index, row := range rows {
			if errors.Is(row.Err, corestore.ErrNotFound) {
				continue
			}
			if row.Err != nil {
				return row.Err
			}
			plan := nativeClaimPlanFromBatch(shared, page.Items[index], row)
			if plan.Item.Revision != plan.Candidate.Revision || plan.Item.Archived {
				continue
			}
			if err := validateClaimSnapshotPolicy(scope, plan); err != nil {
				return err
			}
			scan.plans = append(scan.plans, plan)
		}

		if err := readNativeClaimReleaseDeliveryFacts(ctx, r, scope, scan.plans); err != nil {
			return err
		}
		return readNativeClaimChangeFacts(ctx, r, scope, scan.plans)
	})
	if err == nil {
		plans := scan.plans
		if scan.replay != nil {
			plans = []hubrecords.NativeClaimPlan{*scan.replay}
		}
		err = s.prepareNativeClaimLegacyAuditFacts(ctx, scope, plans)
		if err == nil {
			err = s.prepareNativeClaimApprovals(ctx, scope, plans)
		}
		if scan.replay != nil {
			scan.replay = &plans[0]
		}
	}
	return scan, err
}

func nativeClaimPlanFromBatch(shared hubrecords.NativeClaimPlan, candidate claimstore.Candidate, row tenantstore.ClaimPreviewPlanRow) hubrecords.NativeClaimPlan {
	plan := shared
	plan.SnapshotLoaded = true
	plan.Observations = row.Observations
	plan.ChangePolicy = hubrecords.CurrentChangePolicyRows{Project: shared.Policy}
	if row.ReviewPolicy != nil {
		plan.ChangePolicy.Review = *row.ReviewPolicy
		plan.ChangePolicy.HasReview = true
	}
	plan.Candidate, plan.Item, plan.Version = candidate, row.Item, row.Version
	plan.LinkedRecord = row.LinkedSource
	if row.Change != nil {
		plan.ChangeRecord = *row.Change
	}
	if row.ChangeVersion != nil {
		plan.ChangeVersion = *row.ChangeVersion
	}
	if row.ChangePolicyRevision != nil {
		plan.ChangePolicyRevision = *row.ChangePolicyRevision
	}
	if row.ChangeVersion != nil {
		plan.Change = tracker.NativeChangeReference{ChangeID: plan.ChangeRecord.ID, VersionID: plan.ChangeVersion.ID, HeadSHA: plan.ChangeVersion.HeadSHA}
	}
	if row.LatestAttempt != nil {
		plan.LatestAttempt, plan.LatestAttemptFound = *row.LatestAttempt, true
	}
	if row.LatestRunEvent != nil {
		plan.LatestRunEvent, plan.LatestRunEventFound = *row.LatestRunEvent, true
	}
	plan.LandingPending = nativeClaimPendingLanding(plan)
	return plan
}

func (s *Service) claimNativeIssueTenantWithPlan(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, ttl time.Duration, now time.Time, plan hubrecords.NativeClaimPlan, cursor *claimstore.SelectionCursor) (leasestore.Lease, error) {
	if !nativeClaimIssueContractAllowed(plan) {
		return leasestore.Lease{}, &hubapi.NativeError{Code: "issue_contract_unsatisfied", Message: "Current issue contract does not allow dispatch", Status: http.StatusConflict}
	}
	if (hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending) && !plan.ChangeReady {
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	if len(request.ProviderCandidates) > 0 && !slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) {
		candidate := slices.IndexFunc(request.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool {
			return candidate.WorkItemID == plan.Candidate.WorkItemID
		})
		if candidate < 0 || int64(request.ProviderCandidates[candidate].Revision) != plan.Candidate.Revision {
			return leasestore.Lease{}, providerWait("provider_candidate_changed", "Work item changed after local model selection; refresh before claiming")
		}
	}
	runnerAPI := true
	if plan.Descriptor.Configuration != nil {
		workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(scope.Organization)+"/"+string(scope.Project), plan.Descriptor)
		if err != nil {
			return leasestore.Lease{}, err
		}
		runnerAPI = workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered
	}
	decision := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: plan.Item.NativeID, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{RunnerID: scope.Credential.Runner.RunnerID, Source: "native_claim", Outcome: "claimed", Reason: "A current fenced lease owns this item", At: now, WorkItemRevision: tracker.Revision(plan.Item.Revision)}}}
	if (hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending) && plan.ChangeReady && plan.Change.VersionID != "" {
		decision.Data.Change = new(plan.Change)
	}
	decisionRaw, err := json.Marshal(decision)
	if err != nil {
		return leasestore.Lease{}, err
	}
	decisionRow, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).PrepareEvent(ctx, scope.Organization, decisionRaw)
	if err != nil {
		return leasestore.Lease{}, err
	}
	if plan.Replay == nil && !plan.Source.Loaded {
		plan.Source, err = claimsource.Read(ctx, claimsource.Deps{Repository: s.config.TenantRepository, Details: s.runDetailStore(), Checkpoint: s.readNativeAttemptCheckpointTenant}, scope.Organization, scope.Project, plan.Item)
		if err != nil {
			return leasestore.Lease{}, err
		}
	}
	var cancellation *preparedNativeLeaseCancellation
	if plan.LatestAttemptFound && plan.Replay == nil {
		cancellation, err = s.prepareExpiredNativeLeaseCancellation(ctx, scope, plan.Item.ID, true)
		if err != nil {
			return leasestore.Lease{}, err
		}
	}
	var lease leasestore.Lease
	var isolationRefused bool
	var placementRefusal, sourceRefusal string
	placement := plan.Placement
	if placement == nil {
		placement = &hubrecords.ClaimPlacementSnapshot{}
	}
	write := func(ctx context.Context, w tenantstore.Writer) error {
		placementRefusal, sourceRefusal = "", ""
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		authoritativeNow := s.config.Clock().UTC()
		if err := runnerIdentityClaimRefusal(identity, scope, request.MachineID, authoritativeNow); err != nil {
			return err
		}
		if err := plan.Descriptor.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
			return &hubapi.NativeError{Code: "selector_no_match", Message: err.Error(), Status: http.StatusConflict}
		}
		if identity.State == "active" && identity.CapacityLimit <= 0 {
			machine, err := w.Machine(ctx, string(identity.MachineID))
			if err != nil {
				return err
			}
			if machine.Capacity <= 0 {
				return providerWait("host_capacity", "Runner host has no available execution slots")
			}
			return providerWait("runner_capacity", "Runner has no available execution slots")
		}
		if identity.State != "active" || identity.CapacityConfig == nil || !authoritativeNow.Before(identity.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout)) || authoritativeNow.Before(identity.CapacityConfig.ObservedAt) {
			return ErrNoClaimableWork
		}
		isolationReport, err := w.RunnerBackendIsolation(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		isolationRefused = !isolationReport.SupportsOn(identity.IsolationTier, identity.OS)
		if isolationRefused {
			return ErrNoClaimableWork
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, authoritativeNow)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNoClaimableWork
		}
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if plan.Project.Core.Profile == "github_compatible" || slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) || request.LandingCapacity > 0 || plan.LandingRequired || plan.Issue.IssueContract != nil && plan.Issue.IssueContract.ReleaseDelivery != nil {
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
		}
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !nativeClaimProjectEnabled(project) {
			return ErrNoClaimableWork
		}
		if project.Core.Profile == "github_compatible" && !hubrecords.SameWorkItemProjectPlan(plan.Project, project) {
			return ErrNoClaimableWork
		}
		item, err := w.WorkItemByID(ctx, scope.Project, plan.Candidate.ID)
		if err != nil {
			return err
		}
		if item.NativeID != plan.Candidate.WorkItemID || item.Archived || plan.Replay == nil && (item.Revision != plan.Candidate.Revision || item.DispatchGeneration != plan.Item.DispatchGeneration) {
			return ErrNoClaimableWork
		}
		if project.Core.Profile == "github_compatible" {
			facts, err := w.WorkItemFacts(ctx, scope.Project, item.NativeID)
			if err != nil {
				return err
			}
			candidate := plan.Candidate
			candidate.Repository = facts.Repository
			if !nativeCompatibleClaimSource(project, plan.Issue, candidate) {
				return ErrNoClaimableWork
			}
		}
		currentLinked, err := w.LinkedSource(ctx, scope.Project, item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && (plan.LinkedRecord == nil || !sameNativeClaimLinkedSource(currentLinked, *plan.LinkedRecord)) || errors.Is(err, corestore.ErrNotFound) && plan.LinkedRecord != nil {
			return corestore.ErrRevisionConflict
		}
		workspaceItem, err := w.IsWorkspaceWorkItem(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		workspaceLane := slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability)
		if workspaceItem && !workspaceLane || !workspaceItem && slices.Contains(request.Capabilities, tracker.NativeProviderCapacityCapability) && len(request.ProviderCandidates) == 0 {
			return ErrNoClaimableWork
		}
		if workspaceItem && plan.Replay == nil {
			allowed, err := s.nativeWorkspaceClaimAllowed(ctx, w, scope, item.NativeID, authoritativeNow)
			if err != nil {
				return err
			}
			if !allowed {
				return ErrNoClaimableWork
			}
		}
		item, err = w.LockWorkItem(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		if item.Archived || plan.Replay == nil && (item.Revision != plan.Candidate.Revision || item.DispatchGeneration != plan.Item.DispatchGeneration) {
			return corestore.ErrRevisionConflict
		}
		if plan.Issue.IssueContract != nil && plan.Issue.IssueContract.ReleaseDelivery != nil {
			if item.Revision != plan.Item.Revision {
				return corestore.ErrRevisionConflict
			}
			current := plan
			current.Project = project
			current.ReleaseDelivery, err = readReleaseDeliveryContractFacts(ctx, w, scope, item.NativeID, project.States)
			if err != nil {
				return err
			}
			if !nativeClaimIssueContractAllowed(current) {
				return ErrNoClaimableWork
			}
		}
		replayLease, replayErr := w.ActiveLeaseForIssue(ctx, item.ID)
		if replayErr != nil && !errors.Is(replayErr, corestore.ErrNotFound) {
			return replayErr
		}
		var replayOwner runnerstore.Identity
		if replayErr == nil && replayLease.MachineID == string(identity.MachineID) && replayLease.SessionID == request.SessionID {
			replayOwner, err = w.RunnerForLease(ctx, replayLease.LeaseID)
			if err != nil {
				return err
			}
		}
		replay := plan.Replay
		if replay == nil && replayErr == nil && replayOwner.RunnerID == identity.RunnerID {
			replay = &replayLease
		}
		if replay == nil {
			query := nativeTenantCandidateQuery(scope, request, plan.Candidate.WorkItemID, authoritativeNow)
			page, err := w.CandidatePage(ctx, query)
			if err != nil {
				return err
			}
			if len(page.Items) != 1 || page.Items[0].Revision != plan.Candidate.Revision {
				return ErrNoClaimableWork
			}
			latest, err := w.LatestAttempt(ctx, item.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			current := plan
			current.Item = item
			current.LatestAttempt, current.LatestAttemptFound = latest, err == nil
			if nativeClaimAnswerRequired(current) && current.LatestAttempt.ClaimDisposition == nil {
				return hubapi.NativeRunEventStorageUnavailable()
			}

			if nativeClaimDispatchSatisfied(current) {
				return ErrNoClaimableWork
			}
		}
		if hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending {
			latest, err := w.LatestChange(ctx, scope.Project, plan.Candidate.WorkItemID)
			if err != nil || latest.ID != plan.Change.ChangeID || latest.CurrentVersionID == nil || *latest.CurrentVersionID != plan.Change.VersionID {
				if err != nil {
					return err
				}
				return corestore.ErrRevisionConflict
			}
			current, err := w.LockChange(ctx, plan.ChangeRecord.ID)
			if err != nil {
				return err
			}
			if !sameNativeClaimChangeRecord(current, plan.ChangeRecord) {
				return corestore.ErrRevisionConflict
			}
			version, err := w.Version(ctx, current.ID, plan.Change.VersionID)
			if err != nil {
				return err
			}
			if version.PolicyRevisionID != plan.ChangeVersion.PolicyRevisionID || version.HeadSHA != plan.ChangeVersion.HeadSHA {
				return corestore.ErrRevisionConflict
			}
			versionPolicy, err := w.ProjectPolicyRevision(ctx, scope.Project, version.PolicyRevisionID)
			if err != nil {
				return err
			}
			if !hubrecords.SameNativeClaimPolicyRevision(versionPolicy, plan.ChangePolicyRevision) {
				return corestore.ErrRevisionConflict
			}
			currentPolicy, err := readCurrentChangePolicyRows(ctx, w, scope)
			if err != nil {
				return err
			}
			if !sameNativeClaimChangePolicyRows(currentPolicy, plan.ChangePolicy) {
				return corestore.ErrRevisionConflict
			}
			if err := verifyNativeClaimLandingEvent(ctx, w, item.ID, plan); err != nil {
				return err
			}
		}
		var landingCapacity int64
		if replay != nil && replay.LandingCapacity > 0 {
			landingCapacity = int64(request.LandingCapacity)
		}
		if replay == nil && plan.LandingRequired {
			usage, err := w.LandingCapacityUsage(ctx, scope.Project, authoritativeNow)
			if err != nil {
				return err
			}
			capacity := int64(request.LandingCapacity)
			if usage.Limit > 0 && (capacity <= 0 || usage.Limit < capacity) {
				capacity = usage.Limit
			}
			if usage.Unknown || capacity > 0 && usage.Active >= capacity {
				return ErrNoClaimableWork
			}
			landingCapacity = capacity
		}
		var exposure budget.CostExposure
		joining := ""
		if replay == nil {
			availableCapacity, projectCapacity, projectCapacityConfigured, err := runnerClaimCapacity(identity, scope.Project, authoritativeNow)
			if err != nil {
				return err
			}
			machine, err := w.LockMachine(ctx, string(identity.MachineID))
			if err != nil {
				return err
			}
			if err := runnerVersionError(minimumRunnerVersion(s.config.Version), machine.Version); err != nil {
				return err
			}
			if machine.Capacity <= 0 {
				return providerWait("host_capacity", "Runner host has no available execution slots")
			}
			if availableCapacity <= 0 {
				return providerWait("runner_capacity", "Runner has no available execution slots")
			}
			exposure = monthlyCostExposure(machine, workspaceItem, runnerAPI)
			batch := ""
			if hubapi.StringsEqualFold(plan.Candidate.State, "Merging") && plan.ChangeReady && !plan.LandingRequired {
				parts := strings.Split(request.SessionID, "/")
				if len(parts) == 3 && parts[0] == "landing" && parts[1] != "" && parts[2] != "" {
					batch = parts[1]
				}
			}
			usage, err := w.ClaimCapacityUsage(ctx, machine.ID, identity.RunnerID, scope.Project, batch, authoritativeNow)
			if err != nil {
				return err
			}
			if usage.Host >= machine.Capacity {
				return errClaimHostSlotTaken
			}
			if usage.Runner >= int64(availableCapacity) || projectCapacityConfigured && usage.Project >= int64(projectCapacity) {
				return providerWait("runner_capacity", "Runner has no available execution slots")
			}
			if batch != "" {
				joining = landingCapacityKey(identity.MachineID, string(scope.Project), request.SessionID)
			}
			if sourceRefusal, err = claimsource.Refusal(ctx, w, scope.Project, item, plan.Source, string(identity.MachineID), identity.RunnerID); !workspaceItem && (err != nil || sourceRefusal != "") {
				placementRefusal = sourceRefusal
				return cmp.Or(err, ErrNoClaimableWork)
			}
			sourceRefusal = ""
			allowed, reason, err := claimPlacementAllowed(ctx, w, scope, identity, machine, plan, request.ProviderCandidates, workspaceItem, joining, authoritativeNow, placement.Snapshot)
			if err != nil {
				return err
			}
			if !allowed {
				placementRefusal = reason
				return ErrNoClaimableWork
			}
		} else {
			monthlyLease, err := w.BillingMonthlyLease(ctx, tracker.LeaseID(replay.LeaseID))
			if err == nil {
				exposure = budget.CostExposure{LunaAPI: monthlyLease.LunaAPI, RunnerAPI: monthlyLease.RunnerAPI, SpriteInfrastructure: monthlyLease.SpriteInfrastructure}
			} else if errors.Is(err, corestore.ErrNotFound) {
				machine, err := w.Machine(ctx, string(identity.MachineID))
				if err != nil {
					return err
				}
				exposure = monthlyCostExposure(machine, workspaceItem, runnerAPI)
			} else {
				return err
			}
		}
		if replay == nil && s.database.plansService().Plans() != nil {
			entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, w, authoritativeNow))
			if err != nil {
				return err
			}
			if !slices.Contains(entitlement.Features, "native_execution") {
				return &hostedplans.LimitError{Resource: "native_execution"}
			}
			if limit, limited := entitlement.Allowances["concurrent_work"]; limited {
				populations, ok := w.(billingstore.PopulationReader)
				if !ok {
					return hostedplans.ErrBillingUsageUnavailable
				}
				used, known, err := populations.BillingPopulation(ctx, "concurrent_work", authoritativeNow, 0)
				if err != nil {
					return err
				}
				if used >= limit {
					return &hubapi.NativeError{Code: "plan_capacity", Message: fmt.Sprintf("The plan's concurrent work limit is reached (%d of %d); work stays queued until a slot frees", used, limit), Status: http.StatusConflict, PublicMessage: true}
				}
				if !known {
					return hostedplans.ErrBillingUsageUnavailable
				}
			}
		}
		var selectedReservation *providercapacity.Reservation
		var selectionRequired error
		if !workspaceItem && replay == nil {
			if len(request.ProviderCandidates) == 0 {
				reports, err := w.RunnerProviderReports(ctx, identity.RunnerID)
				if err != nil {
					return err
				}
				if len(reports) > 0 {
					selectionRequired = providerWait("provider_incompatible", "Work item needs local model selection before a provider reservation can be claimed")
				}
			} else {
				reservation, err := selectTenantProviderReservation(ctx, w, identity, request, item.NativeID, joining, authoritativeNow)
				if err != nil {
					return err
				}
				selectedReservation = &reservation
			}
		}
		leaseID := hubapi.NewID("lease")
		lease, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(identity.MachineID),
			SessionID: request.SessionID, LeaseID: leaseID, TTL: ttl, LandingCapacity: landingCapacity, RunnerID: identity.RunnerID, PreviousAttemptCancellation: cancellation.attemptCancellation()})
		if err != nil {
			return err
		}
		currentPolicy, err := w.ProjectPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubrecords.ErrNoApprovedRepositoryPolicy
		}
		if err != nil {
			return err
		}
		if !hubrecords.SameNativeClaimPolicyRevision(currentPolicy, plan.Policy) || currentPolicy.ApprovedBy != plan.Policy.ApprovedBy || !currentPolicy.ApprovedAt.Equal(plan.Policy.ApprovedAt) {
			return corestore.ErrRevisionConflict
		}
		if lease.LeaseID != leaseID {
			if replayLease.LeaseID != lease.LeaseID || replayOwner.RunnerID != identity.RunnerID {
				return tracker.ErrLeaseConflict
			}
			pinned, err := w.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
			if err != nil {
				return err
			}
			if pinned.PolicyID != plan.Policy.PolicyID {
				return tracker.ErrLeaseConflict
			}
			reservation, foundReservation, err := readTenantProviderReservation(ctx, w, tracker.LeaseID(lease.LeaseID))
			if err != nil {
				return err
			}
			if !workspaceItem && (len(request.ProviderCandidates) > 0 || slices.Contains(request.Capabilities, tracker.NativeProviderCapacityCapability)) {
				candidate := nativeClaimProviderCandidate(request, item.NativeID)
				if !foundReservation || candidate == nil || reservation.Reservation.Requirement != candidate.Requirement {
					return tracker.ErrLeaseConflict
				}
			}
		} else {
			if _, err := w.AppendEvent(ctx, decisionRow); err != nil {
				return err
			}
			selector := plan.Descriptor.Requirements
			selector.RequiredTags = slices.Clone(selector.RequiredTags)
			if err := w.BindLeasePolicy(ctx, policystore.LeasePolicy{OrganizationID: scope.Organization, ProjectID: scope.Project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: plan.Policy.PolicyID, BoundAt: authoritativeNow, Selector: &selector}); err != nil {
				return err
			}
			if selectedReservation != nil {
				row := claimstore.ProviderReservation{OrganizationID: scope.Organization, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: *selectedReservation}
				if err := w.CreateProviderReservation(ctx, row); err != nil {
					return err
				}
			}
		}
		input, err := readBillingMonthlyBudgetInput(ctx, w, scope, authoritativeNow)
		if err != nil {
			return err
		}
		_, err = w.BillingMonthlyAdmission(ctx, tracker.WorkItemID(item.ID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		input.Exposure, input.Admitted = exposure, err == nil
		decision, err := billingstore.EvaluateMonthlyBudget(input)
		if err != nil {
			return err
		}
		if !decision.Allowed {
			return &hubapi.NativeError{Code: "provider_capacity", Message: monthlyBudgetRefusal(decision), Status: http.StatusConflict, Details: map[string]any{"monthly_budget": decision}}
		}
		if selectionRequired != nil {
			return selectionRequired
		}
		if lease.LeaseID == leaseID {
			if err := w.RecordBillingMonthlyAdmission(ctx, billingstore.MonthlyAdmission{OrganizationID: scope.Organization, IssueID: tracker.WorkItemID(item.ID), AdmittedAt: authoritativeNow}); err != nil {
				return err
			}
			if err := w.RecordBillingMonthlyLease(ctx, billingstore.MonthlyLease{OrganizationID: scope.Organization, LeaseID: tracker.LeaseID(lease.LeaseID), LunaAPI: exposure.LunaAPI, RunnerAPI: exposure.RunnerAPI, SpriteInfrastructure: exposure.SpriteInfrastructure}); err != nil {
				return err
			}
		}
		if cursor != nil {
			written, err := w.PutSelectionCursor(ctx, *cursor, cursor.Revision)
			if err != nil {
				return err
			}
			if !written {
				return ErrNoClaimableWork
			}
		}
		return nil
	}
	err = s.writeNativeClaimCommand(ctx, scope, write)
	if errors.Is(err, errClaimPlacementSnapshotRequired) && placement.Snapshot == nil {
		snapshot, snapshotErr := s.readPlacementSnapshot(ctx, scope, now, request.ProviderCandidates)
		if snapshotErr != nil {
			return leasestore.Lease{}, snapshotErr
		}
		placement.Snapshot = &snapshot
		err = s.writeNativeClaimCommand(ctx, scope, write)
	}
	if placementRefusal != "" && errors.Is(err, ErrNoClaimableWork) {
		s.config.Logger.DebugContext(ctx, "native claim candidate skipped",
			"organization_id", scope.Organization, "project_id", scope.Project,
			"work_item_id", plan.Candidate.WorkItemID, "runner_id", scope.Credential.Runner.RunnerID, "reason", placementRefusal)
	}
	if sourceRefusal != "" && errors.Is(err, ErrNoClaimableWork) {
		if recordErr := s.recordClaimRoutingRefusal(ctx, scope, request, now, &claimRoutingRefusal{code: "source_unavailable", reason: sourceRefusal, sourceFacts: plan.Source}); recordErr != nil {
			return leasestore.Lease{}, recordErr
		}
	}
	if errors.Is(err, ErrNoClaimableWork) && isolationRefused {
		if recordErr := s.recordRequestedClaimRefusal(ctx, scope, request, now, false, "no_claimable_work", nil); recordErr != nil {
			return leasestore.Lease{}, recordErr
		}
	}
	return lease, err
}

func (s *Service) writeNativeClaimCommand(ctx context.Context, scope hubscope.NativeScope, write func(context.Context, tenantstore.Writer) error) error {
	if command, ok := s.config.TenantRepository.(interface {
		WriteRunnerCommand(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	}); ok {
		return command.WriteRunnerCommand(ctx, scope.Organization, scope.Credential.ID, scope.Credential.Runner.RunnerID, write)
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
}

func (s *Service) persistNativeClaimCursor(ctx context.Context, scope hubscope.NativeScope, cursor claimstore.SelectionCursor) error {
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		authoritativeNow := s.config.Clock().UTC()
		if !runnerIdentityCanClaim(identity, scope, scope.Credential.Runner.MachineID, authoritativeNow) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, authoritativeNow)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNoClaimableWork
		}
		written, err := w.PutSelectionCursor(ctx, cursor, cursor.Revision)
		if err != nil {
			return err
		}
		if !written {
			return ErrNoClaimableWork
		}
		return nil
	})
}

func claimCandidateCursor(candidate claimstore.Candidate) claimstore.CandidateCursor {
	return claimstore.CandidateCursor{Priority: candidate.Priority, StateRank: candidate.StateRank, LabelRank: candidate.LabelRank, AgingBoost: candidate.AgingBoost, LandingRank: candidate.LandingRank, LaneRank: candidate.LaneRank, UnblockerRank: candidate.UnblockerRank, Rank: candidate.Rank, Created: candidate.CreatedAt, WorkItemID: candidate.WorkItemID, SourceRevision: candidate.Revision}
}

func nativeClaimSelectionSourceRevision(project projectstore.Record, policy policystore.Revision, query claimstore.CandidateQuery) (string, error) {
	query.AvailableAt = time.Time{}
	query.After = claimstore.CandidateCursor{}
	query.AfterWorkItemID = ""
	query.Limit = 0
	query.IncludeLeasedAnchor = false
	query.IncludeUnclassifiedSource = false
	var settingsRevision, contentRevision int64
	if project.Core.Profile == "github_compatible" {
		settingsRevision = project.Settings.Revision
		contentRevision = project.Core.ContentSettingsRevision
	}
	return hubapi.CommandHash(struct {
		WorkflowSourceRevision        string
		PolicyID                      string
		PolicySHA256                  string
		Query                         claimstore.CandidateQuery
		CompatibilitySettingsRevision int64 `json:"compatibility_settings_revision,omitempty"`
		CompatibilityContentRevision  int64 `json:"compatibility_content_revision,omitempty"`
		CandidateOrder                string
	}{project.Core.WorkflowSourceRevision, policy.PolicyID, policy.PayloadSHA256, query, settingsRevision, contentRevision, "created_before_preferences"})
}

func selectTenantProviderReservation(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, request tracker.NativeClaim, item tracker.NativeWorkItemID, joining string, now time.Time) (providercapacity.Reservation, error) {
	candidate := nativeClaimProviderCandidate(request, item)
	if candidate == nil || candidate.Revision <= 0 || candidate.Requirement.Validate() != nil {
		return providercapacity.Reservation{}, providerWait("provider_candidate_changed", "Work item needs a current local provider selection before it can be claimed")
	}
	reports, err := w.RunnerProviderReports(ctx, identity.RunnerID)
	if err != nil {
		return providercapacity.Reservation{}, err
	}
	var selected *providercapacity.Report
	for index := range reports {
		if reports[index].Supports(candidate.Requirement) {
			selected = &reports[index]
			break
		}
	}
	if selected == nil {
		return providercapacity.Reservation{}, providerWait("provider_incompatible", "Runner does not advertise the selected backend and model")
	}
	capacity, err := w.LockRunnerProviderAccount(ctx, runnerstore.ProviderAccountKey{RunnerID: identity.RunnerID, Provider: selected.Provider, Backend: selected.Backend, AccountAlias: selected.AccountAlias, SharedAccountAlias: selected.SharedAccountAlias}, now)
	if err != nil {
		return providercapacity.Reservation{}, err
	}
	if !capacity.SelectedReport {
		return providercapacity.Reservation{}, providerWait("provider_incompatible", "Runner no longer advertises the selected backend and model")
	}
	group := ""
	if joining != "" && candidate.Requirement.Backend == "git" && candidate.Requirement.Role == "merge" {
		group = joining + "/" + identity.RunnerID
	}
	used, err := w.ProviderReservationUsage(ctx, *selected, identity.RunnerID, group, now)
	if err != nil {
		return providercapacity.Reservation{}, err
	}
	if capacity.MaxConcurrent > 0 && used >= capacity.MaxConcurrent {
		return providercapacity.Reservation{}, providerWait("provider_capacity", "Shared provider concurrency is fully reserved")
	}
	return providercapacity.Reservation{Requirement: candidate.Requirement, Report: *selected, Reason: "Current provider account capacity is available"}, nil
}

func nativeClaimProviderCandidate(request tracker.NativeClaim, item tracker.NativeWorkItemID) *tracker.NativeCapacityCandidate {
	for index := range request.ProviderCandidates {
		if request.ProviderCandidates[index].WorkItemID == item {
			return &request.ProviderCandidates[index]
		}
	}
	return nil
}

func readTenantProviderReservation(ctx context.Context, r claimstore.Reader, lease tracker.LeaseID) (claimstore.ProviderReservation, bool, error) {
	reservation, err := r.ProviderReservation(ctx, lease)
	if errors.Is(err, corestore.ErrNotFound) {
		return claimstore.ProviderReservation{}, false, nil
	}
	if err != nil {
		return claimstore.ProviderReservation{}, false, err
	}
	return reservation, true, nil
}

func (s *Service) readNativeClaimPlan(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time) (hubrecords.NativeClaimPlan, error) {
	plan, err := s.readNativeClaimPlanRows(ctx, scope, request, now, nil, nil, hubrecords.NativeClaimPlan{})
	if err != nil {
		return hubrecords.NativeClaimPlan{}, err
	}
	if err := s.prepareNativeClaimLegacyAuditFacts(ctx, scope, []hubrecords.NativeClaimPlan{plan}); err != nil {
		return hubrecords.NativeClaimPlan{}, err
	}
	plans := []hubrecords.NativeClaimPlan{plan}
	if err := s.prepareNativeClaimApprovals(ctx, scope, plans); err != nil {
		return hubrecords.NativeClaimPlan{}, err
	}
	return s.hydrateNativeClaimPlan(ctx, scope, request, now, plans[0], nil)
}

func (s *Service) readNativeClaimPlanRows(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, reader tenantstore.Reader, candidate *claimstore.Candidate, shared hubrecords.NativeClaimPlan) (hubrecords.NativeClaimPlan, error) {
	plan := shared
	readRows := func(r tenantstore.Reader) error {
		var err error
		if plan.Project.Core.ID == "" {
			plan.Project, err = r.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if !nativeClaimProjectEnabled(plan.Project) {
				return ErrNoClaimableWork
			}
			plan.Policy, err = r.ProjectPolicy(ctx, scope.Project)
			if errors.Is(err, corestore.ErrNotFound) {
				return missingClaimPolicyRefusal(ctx, r, scope.Project, request.PolicyID)
			}
			if err != nil {
				return err
			}
			if plan.Policy.PolicyID != request.PolicyID {
				return claimPolicyRefusal(ctx, r, scope.Project, request.PolicyID)
			}
		}
		if request.SessionID != "" && plan.Replay == nil {
			lease, err := r.LeaseBySession(ctx, request.SessionID)
			if err == nil {
				plan.Replay = &lease
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if plan.Replay != nil {
			lease := plan.Replay
			if lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.MachineID != string(request.MachineID) {
				return tracker.ErrLeaseConflict
			}
			if candidate == nil {
				item, err := r.WorkItemByID(ctx, scope.Project, lease.IssueID)
				if err != nil {
					return err
				}
				value := claimstore.Candidate{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: item.ID, Revision: item.Revision, State: hubapi.ValueOrEmpty(item.WorkflowStateName), CreatedAt: item.NativeCreatedAt}
				candidate = &value
			}
			if candidate.ID != lease.IssueID || request.WorkItemID != "" && request.WorkItemID != candidate.WorkItemID {
				return tracker.ErrLeaseConflict
			}
			if len(request.ProviderCandidates) > 0 && !slices.ContainsFunc(request.ProviderCandidates, func(c tracker.NativeCapacityCandidate) bool {
				return c.WorkItemID == candidate.WorkItemID && int64(c.Revision) == candidate.Revision
			}) {
				return tracker.ErrLeaseConflict
			}
		}
		if candidate == nil {
			page, err := r.CandidatePage(ctx, nativeTenantCandidateQuery(scope, request, request.WorkItemID, now))
			if err != nil {
				return err
			}
			if len(page.Items) == 0 {
				return ErrNoClaimableWork
			}
			candidate = &page.Items[0]
		}
		if plan.Project.Core.Profile == "github_compatible" && candidate.Repository == "" {
			facts, err := r.WorkItemFacts(ctx, scope.Project, candidate.WorkItemID)
			if err != nil {
				return err
			}
			selected := *candidate
			selected.Repository = facts.Repository
			candidate = &selected
		}
		batch, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		rows, err := batch.ClaimPreviewPlanRows(ctx, scope.Project, []claimstore.Candidate{*candidate})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return corestore.ErrRevisionConflict
		}
		if rows[0].Err != nil {
			return rows[0].Err
		}
		if rows[0].Item.Revision != candidate.Revision || rows[0].Item.Archived {
			return ErrNoClaimableWork
		}
		plan = nativeClaimPlanFromBatch(plan, *candidate, rows[0])
		if err := validateClaimSnapshotPolicy(scope, plan); err != nil {
			return err
		}
		plans := []hubrecords.NativeClaimPlan{plan}
		if err := readNativeClaimChangeFacts(ctx, r, scope, plans); err != nil {
			return err
		}
		if err := readNativeClaimReleaseDeliveryFacts(ctx, r, scope, plans); err != nil {
			return err
		}
		plan = plans[0]
		return nil
	}
	var err error
	if reader != nil {
		err = readRows(reader)
	} else {
		repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
		if !ok {
			return plan, hubapi.NativeRunEventStorageUnavailable()
		}
		err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			if err := readRows(r); err != nil {
				return err
			}
			if plan.Replay == nil {
				plan.Ranking, err = readNativeClaimRanking(ctx, r, scope)
			}
			return err
		})
	}
	return plan, err
}

func (s *Service) prepareNativeClaimApprovals(ctx context.Context, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	var approval *hubrecords.ChangeVersionApproval
	for i := range plans {
		if (hubapi.StringsEqualFold(plans[i].Candidate.State, "Merging") || plans[i].LandingPending) && plans[i].ChangeRecord.ID != "" {
			if approval == nil {
				value, err := hydrateChangeSummaryPolicy(ctx, eventstore.NewContent(s.runDetailStoreFor(ctx)), scope, plans[i].ChangePolicy)
				if err != nil {
					return err
				}
				approval = &value
			}
			plans[i].Approval = approval
		}
	}
	return nil
}

func validateClaimSnapshotPolicy(scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan) error {
	project := plan.ChangePolicy.Project
	if project.OrganizationID != scope.Organization || project.ProjectID != scope.Project || project.PolicyID == "" || project.ApprovedBy == "" || project.ApprovedAt.IsZero() {
		return corestore.ErrTenantMismatch
	}
	if plan.ChangePolicy.HasReview {
		review := plan.ChangePolicy.Review
		if review.OrganizationID != scope.Organization || review.ProjectID != scope.Project || review.PolicyRevisionID == "" || review.ReviewPolicyID == "" || review.Revision < 1 || review.UpdatedBy == "" || review.UpdatedAt.IsZero() {
			return corestore.ErrTenantMismatch
		}
	}
	return nil
}

func (s *Service) hydrateNativeClaimPlan(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, now time.Time, plan hubrecords.NativeClaimPlan, descriptor *policy.Descriptor) (hubrecords.NativeClaimPlan, error) {
	if !plan.SnapshotLoaded {
		return plan, hubapi.NativeRunEventStorageUnavailable()
	}
	if plan.Replay == nil && slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			allowed, err := s.nativeWorkspaceClaimAllowed(ctx, r, scope, plan.Item.NativeID, now)
			if err != nil {
				return err
			}
			if !allowed {
				return ErrNoClaimableWork
			}
			return nil
		}); err != nil {
			return hubrecords.NativeClaimPlan{}, err
		}
	}
	if nativeClaimAnswerRequired(plan) && plan.LatestAttempt.ClaimDisposition == nil {
		return hubrecords.NativeClaimPlan{}, hubapi.NativeRunEventStorageUnavailable()
	}
	var err error
	if descriptor == nil && plan.Approval != nil {
		descriptor = &plan.Approval.Policy
	}
	if descriptor == nil {
		loaded, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, plan.Policy)
		if err != nil {
			return hubrecords.NativeClaimPlan{}, err
		}
		descriptor = &loaded
	}
	plan.Descriptor = *descriptor
	if plan.Descriptor.ID != request.PolicyID {
		return hubrecords.NativeClaimPlan{}, hubapi.PolicyMismatch("Claim configuration does not match the current project policy")
	}
	if plan.Policy.GitHubPullRequest == nil || *plan.Policy.GitHubPullRequest != plan.Descriptor.Gates.GitHubPullRequest {
		return hubrecords.NativeClaimPlan{}, errNativeClaimPlanRefused
	}
	raw, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).CollaborationVersion(ctx, plan.Version)
	if err != nil {
		return hubrecords.NativeClaimPlan{}, err
	}
	if err := json.Unmarshal(raw, &plan.Issue); err != nil || plan.Issue.OrganizationID != plan.Item.OrganizationID || plan.Issue.ProjectID != plan.Item.ProjectID || plan.Issue.WorkItemID != plan.Item.NativeID || int64(plan.Issue.Revision) != plan.Item.Revision || int64(plan.Issue.Number) != plan.Item.Number || plan.Issue.Title != plan.Item.Title || plan.Issue.Archived != plan.Item.Archived || plan.Issue.Actor.Kind != hubapi.ValueOrEmpty(plan.Item.ActorKind) || plan.Issue.Actor.PrincipalID != hubapi.ValueOrEmpty(plan.Item.ActorPrincipalID) {
		return hubrecords.NativeClaimPlan{}, corestore.ErrRevisionConflict
	}

	if nativeClaimDispatchSatisfied(plan) {
		return hubrecords.NativeClaimPlan{}, errNativeClaimPlanRefused
	}
	if exclusion, err := s.nativeClaimAnsweredExclusion(ctx, scope, plan, now); err != nil {
		return hubrecords.NativeClaimPlan{}, err
	} else if exclusion.Kind != "" {
		return hubrecords.NativeClaimPlan{}, errNativeClaimPlanRefused
	}
	plan.Issue.State = hubapi.ValueOrEmpty(plan.Item.WorkflowStateName)
	plan.Issue.LastActivityAt = plan.Item.LastActivityAt
	if plan.LinkedRecord != nil {
		record := plan.LinkedRecord
		linked := tracker.LinkedIssueSource{URL: record.URL, Status: "pending"}
		if record.SnapshotObjectKey != "" {
			snapshot, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).LinkedIssueSnapshot(ctx, scope.Organization, eventstore.Reference(record.SnapshotObjectKey, record.SnapshotSHA256, record.SnapshotBytes, record.SnapshotContentBytes))
			if err != nil {
				return hubrecords.NativeClaimPlan{}, err
			}
			if snapshot.URL != record.URL {
				return hubrecords.NativeClaimPlan{}, artifact.ErrIntegrity
			}
			linked.Status, linked.Snapshot = "complete", &snapshot
		}
		plan.Linked = &linked
		plan.Issue.LinkedSource = &linked
	}
	if plan.Project.Core.Profile == "github_compatible" && !nativeCompatibleClaimSource(plan.Project, plan.Issue, plan.Candidate) {
		return hubrecords.NativeClaimPlan{}, errNativeClaimPlanRefused
	}
	if hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending {
		if plan.ChangeRecord.ID == "" || plan.ChangeRecord.CurrentVersionID == nil || *plan.ChangeRecord.CurrentVersionID != plan.ChangeVersion.ID || plan.ChangeVersion.ID != plan.Change.VersionID || plan.ChangeVersion.HeadSHA != plan.Change.HeadSHA {
			return plan, nil
		}
		detail, err := s.nativeClaimCurrentChangeDetail(ctx, scope, plan, now)
		if err != nil {
			return hubrecords.NativeClaimPlan{}, err
		}
		state := changerequest.ReviewState(detail, plan.ChangeVersion.ID, plan.ChangeVersion.HeadSHA)
		currentVersion := hubapi.ValueOrEmpty(plan.ChangeRecord.CurrentVersionID)
		plan.ChangeReady = plan.ChangeRecord.OrganizationID == scope.Organization && plan.ChangeRecord.ProjectID == scope.Project && plan.ChangeRecord.WorkItemID == plan.Candidate.WorkItemID && detail.Change.ID == plan.ChangeRecord.ID && int64(detail.Change.Revision) == plan.ChangeRecord.Revision && detail.Change.CurrentVersion == currentVersion && currentVersion == plan.ChangeVersion.ID && state.Current && !state.ChangesRequested && !state.RepairPending && nativeChangeLandingClaimable("Merging", &detail)
	}
	plan.LandingRequired, err = nativeClaimLandingCapacityRequired(plan)
	if err != nil {
		return hubrecords.NativeClaimPlan{}, err
	}
	return plan, nil
}

func nativeClaimLandingCapacityRequired(plan hubrecords.NativeClaimPlan) (bool, error) {
	if hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending {
		if plan.ChangePolicyRevision.GitHubPullRequest == nil {
			return false, ErrNoClaimableWork
		}
		return *plan.ChangePolicyRevision.GitHubPullRequest, nil
	}
	return false, nil
}

func nativeClaimPendingLanding(plan hubrecords.NativeClaimPlan) bool {
	if hubapi.StringsEqualFold(plan.Candidate.State, "Rework") || !plan.LatestAttemptFound || !plan.LatestRunEventFound || !plan.LatestRunEvent.LandingWaiting || plan.ChangeRecord.ID == "" || plan.ChangeRecord.CurrentVersionID == nil {
		return false
	}
	if plan.LatestAttempt.WorkItemRevision < plan.Item.Revision || plan.LatestAttempt.DispatchGeneration < plan.Item.DispatchGeneration {
		return false
	}
	return plan.LatestRunEvent.LandingChangeID == plan.ChangeRecord.ID && plan.LatestRunEvent.LandingVersionID == plan.ChangeVersion.ID && plan.LatestRunEvent.LandingHeadSHA == plan.ChangeVersion.HeadSHA && plan.ChangeVersion.ID == *plan.ChangeRecord.CurrentVersionID
}

func verifyNativeClaimLandingEvent(ctx context.Context, r tenantstore.Reader, issueID int64, plan hubrecords.NativeClaimPlan) error {
	attempt, err := r.LatestAttempt(ctx, issueID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		if plan.LatestAttemptFound {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !plan.LatestAttemptFound || attempt.ID != plan.LatestAttempt.ID || attempt.FencingToken != plan.LatestAttempt.FencingToken || attempt.Sequence != plan.LatestAttempt.Sequence {
		return corestore.ErrRevisionConflict
	}
	event, err := r.LatestRunEvent(ctx, plan.Candidate.ProjectID, attempt.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		if plan.LatestRunEventFound {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !plan.LatestRunEventFound || event.ID != plan.LatestRunEvent.ID || event.Sequence != plan.LatestRunEvent.Sequence || event.LandingWaiting != plan.LatestRunEvent.LandingWaiting || event.LandingChangeID != plan.LatestRunEvent.LandingChangeID || event.LandingVersionID != plan.LatestRunEvent.LandingVersionID || event.LandingHeadSHA != plan.LatestRunEvent.LandingHeadSHA {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func sameNativeClaimChangeRecord(left, right eventstore.Change) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.WorkItemID == right.WorkItemID && left.ID == right.ID && left.Revision == right.Revision && left.Title == right.Title && hubapi.ValueOrEmpty(left.CurrentVersionID) == hubapi.ValueOrEmpty(right.CurrentVersionID) && left.EvidenceSequence == right.EvidenceSequence && left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimChangePolicyRows(left, right hubrecords.CurrentChangePolicyRows) bool {
	return hubrecords.SameNativeClaimProjectPolicy(left.Project, right.Project) && sameNativeClaimReviewPolicy(left.Review, right.Review) && left.HasReview == right.HasReview
}

func sameNativeClaimReviewPolicy(left eventstore.ReviewPolicy, right eventstore.ReviewPolicy) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyRevisionID == right.PolicyRevisionID && left.ReviewPolicyID == right.ReviewPolicyID && left.Revision == right.Revision && left.UpdatedBy == right.UpdatedBy && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimLinkedSource(left, right eventstore.LinkedSource) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.WorkItemID == right.WorkItemID && left.URL == right.URL && left.TitleSupplied == right.TitleSupplied && left.BodySupplied == right.BodySupplied && left.TitleOwned == right.TitleOwned && left.BodyOwned == right.BodyOwned && left.SnapshotObjectKey == right.SnapshotObjectKey && left.SnapshotSHA256 == right.SnapshotSHA256 && left.SnapshotBytes == right.SnapshotBytes && left.SnapshotContentBytes == right.SnapshotContentBytes && hubapi.StringValue(left.ExpectedSnapshotSHA256) == hubapi.StringValue(right.ExpectedSnapshotSHA256)
}

func (s *Service) nativeWorkspaceClaimAllowed(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID, now time.Time) (bool, error) {
	if s.workspaces == nil {
		return false, nil
	}
	owner, ok := r.(postgres.WorkspaceReader)
	if !ok {
		return false, hubapi.WorkspaceUnavailable()
	}
	unavailable := func(source string) (bool, error) {
		s.config.Logger.WarnContext(ctx, "workspace claim candidate skipped", "organization_id", scope.Organization, "project_id", scope.Project, "work_item_id", item, "source", source)
		return false, nil
	}
	row, err := owner.WorkspaceByDispatchItem(ctx, scope.Project, item)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	link, err := owner.WorkspaceDispatchItem(ctx, scope.Project, row.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		return unavailable("workspace_association")
	}
	if err != nil {
		return false, err
	}
	if link.WorkItemID != item || link.ClosedAt != nil || row.State != workspacesession.StateRequested {
		return false, nil
	}
	identity, err := r.RunnerIdentity(ctx, scope.Credential.Runner.RunnerID)
	if err != nil {
		return false, err
	}
	capabilities, isolation, err := r.RunnerWorkspaceReport(ctx, identity.RunnerID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	retained := ""
	if row.AttemptID != "" {
		attempt, err := r.Attempt(ctx, row.AttemptID)
		if errors.Is(err, corestore.ErrNotFound) {
			return unavailable("attempt")
		}
		if err != nil {
			return false, err
		}
		subject, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(row.SubjectWorkItemID))
		if errors.Is(err, corestore.ErrNotFound) {
			return unavailable("subject")
		}
		if err != nil {
			return false, err
		}
		if attempt.OrganizationID != scope.Organization || attempt.IssueID != subject.ID {
			return false, corestore.ErrTenantMismatch
		}
		retainedWindow := now.Before(attempt.UpdatedAt.Add(s.workspaces.config.RetainAfterRun))
		if attempt.Status == "running" || retainedWindow {
			lease, err := r.Lease(ctx, attempt.LeaseID)
			if errors.Is(err, corestore.ErrNotFound) {
				return unavailable("lease")
			}
			if err != nil {
				return false, err
			}
			if lease.IssueID != subject.ID || lease.FencingToken != attempt.FencingToken {
				return false, corestore.ErrRevisionConflict
			}
			if retainedWindow || lease.ReleasedAt == nil && lease.ExpiresAt.After(now) {
				runner, err := r.RunnerForLease(ctx, attempt.LeaseID)
				if errors.Is(err, corestore.ErrNotFound) {
					return unavailable("attempt_runner")
				}
				if err != nil {
					return false, err
				}
				retained = runner.RunnerID
			}
		}
	}
	fresh := !now.Before(identity.LastHeartbeatAt) && now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout))
	allowed, _ := workspaceClaimable(workspaceTenantRecord(row), capabilities, fresh, identity.RunnerID, retained, isolation, s.workspaces.config.Terminal.Isolation)
	return allowed, nil
}

func nativeClaimIssueContractAllowed(plan hubrecords.NativeClaimPlan) bool {
	evaluation, err := nativeRuntimeIssueContractEvaluation(plan)
	return err == nil && evaluation.Allowed
}

func nativeTenantCandidateQuery(scope hubscope.NativeScope, request tracker.NativeClaim, item tracker.NativeWorkItemID, now time.Time) claimstore.CandidateQuery {
	workItemIDs := make([]tracker.NativeWorkItemID, 0, len(request.ProviderCandidates))
	for _, candidate := range request.ProviderCandidates {
		workItemIDs = append(workItemIDs, candidate.WorkItemID)
	}
	return claimstore.CandidateQuery{
		ProjectID: scope.Project, Scope: string(scope.Project), WorkItemID: item, WorkItemIDs: workItemIDs,
		States: request.WorkflowStates, Authors: request.Authors, Assignees: request.Assignees,
		IncludeLabels: request.LabelInclude, ExcludeLabels: request.LabelExclude,
		WorkspaceLane:   slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability),
		RunnerSelection: true, PrioritizeUnblockers: request.PrioritizeUnblockers,
		StatePriority: request.DispatchPriorityByState, LabelPriority: request.DispatchPriorityByLabel,
		AvailableAt: now, Limit: 1,
	}
}

func runnerIdentityClaimRefusal(identity runnerstore.Identity, scope hubscope.NativeScope, machine tracker.MachineID, now time.Time) error {
	if runnerIdentityCanClaim(identity, scope, machine, now) {
		return nil
	}
	if identity.OrganizationID == scope.Organization && identity.TokenID == scope.Credential.ID && identity.RunnerID == scope.Credential.Runner.RunnerID && identity.RemovedAt == nil {
		switch identity.State {
		case "disabled":
			return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
		case "draining":
			return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_draining", Message: "Runner is draining; active leases may finish"}})
		case "active":
			if identity.MachineID == machine && machine == scope.Credential.Runner.MachineID && identity.ExpiresAt.After(now) && slices.Contains(identity.Operations, runnerauth.Claim) {
				return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_offline", Message: "Runner heartbeat is stale; work stays queued for this target"}})
			}
		}
	}
	return tracker.ErrMachineNotFound
}

func runnerIdentityCanClaim(identity runnerstore.Identity, scope hubscope.NativeScope, machine tracker.MachineID, now time.Time) bool {
	return identity.OrganizationID == scope.Organization && identity.TokenID == scope.Credential.ID && identity.RunnerID == scope.Credential.Runner.RunnerID && identity.MachineID == machine && machine == scope.Credential.Runner.MachineID && identity.State == "active" && identity.RemovedAt == nil && identity.ExpiresAt.After(now) && !now.Before(identity.LastHeartbeatAt) && now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) && slices.Contains(identity.Operations, runnerauth.Claim)
}

func nativeClaimProjectEnabled(project projectstore.Record) bool {
	if project.Core.DeletedAt != nil {
		return false
	}
	return project.Core.Profile == "native" || project.Core.Profile == "github_compatible" && project.Settings.RepositoryEnabled && strings.TrimSpace(project.Settings.Repository) != ""
}

func nativeCompatibleClaimSource(project projectstore.Record, issue tracker.NativeIssue, candidate claimstore.Candidate) bool {
	if project.Core.Profile != "github_compatible" || !nativeClaimProjectEnabled(project) || issue.Profile != "github_compatible" || issue.OrganizationID != candidate.OrganizationID || issue.ProjectID != project.Core.ID || issue.ProjectID != candidate.ProjectID || issue.WorkItemID != candidate.WorkItemID || int64(issue.Revision) != candidate.Revision {
		return false
	}
	repository, number, err := cutoverSourceReference(issue)
	if err != nil && issue.LinkedSource != nil && issue.LinkedSource.Status == "complete" {
		_, repository, number, err = tracker.ParseGitHubIssueURL(issue.LinkedSource.URL)
	}
	return err == nil && number > 0 && strings.EqualFold(repository, strings.TrimSpace(project.Settings.Repository)) && strings.EqualFold(repository, candidate.Repository)
}

func missingClaimPolicyRefusal(ctx context.Context, r policystore.Reader, project tracker.ProjectID, policyID string) error {
	if _, err := r.ProjectPolicyRevision(ctx, project, policyID); errors.Is(err, corestore.ErrNotFound) {
		return hubapi.NativePublicInvalid("Claim configuration has not been reported for this project")
	} else if err != nil {
		return err
	}
	return hubrecords.ErrNoApprovedRepositoryPolicy
}

func claimPolicyRefusal(ctx context.Context, r policystore.Reader, project tracker.ProjectID, policyID string) error {
	if policyID != "" {
		_, err := r.ProjectPolicyRevision(ctx, project, policyID)
		if err == nil {
			return hubapi.PolicyMismatch("Claim configuration changed; load the current project policy before claiming")
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
	}
	return hubapi.NativePublicInvalid("Claim configuration has not been reported for this project")
}
