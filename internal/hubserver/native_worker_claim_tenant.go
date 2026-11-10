package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeClaimPlan struct {
	snapshotLoaded       bool
	observations         *tenantstore.ChangeVersionRows
	approval             *ChangeVersionApproval
	candidate            claimstore.Candidate
	project              projectstore.Record
	policy               policystore.Revision
	descriptor           policy.Descriptor
	item                 corestore.WorkItem
	issue                tracker.NativeIssue
	version              eventstore.CollaborationVersion
	linked               *tracker.LinkedIssueSource
	linkedRecord         *eventstore.LinkedSource
	change               tracker.NativeChangeReference
	changeReady          bool
	changeFacts          *tenantstore.BoardChangeRows
	changeRecord         eventstore.Change
	changePolicy         currentChangePolicyRows
	changeVersion        eventstore.Version
	changePolicyRevision policystore.Revision
	latestAttempt        leasestore.Attempt
	latestRunEvent       eventstore.Event
	latestAttemptFound   bool
	latestRunEventFound  bool
	landingPending       bool
	landingRequired      bool
	replay               *leasestore.Lease
}

const nativeClaimCandidatePageSize = 100

type nativeClaimCandidateScan struct {
	plans    []nativeClaimPlan
	replay   *nativeClaimPlan
	pageNext claimstore.CandidateCursor
	cursor   claimstore.SelectionCursor
}

func (s *Service) claimNativeIssueTenant(ctx context.Context, scope nativeScope, request tracker.NativeClaim, ttl time.Duration) (leasestore.Lease, error) {
	if s.config.TenantRepository == nil || s.config.NativeRunEvents == nil || scope.credential.Runner.RunnerID == "" {
		return leasestore.Lease{}, nativeRunEventStorageUnavailable()
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
		return leasestore.Lease{}, nativeRunEventStorageUnavailable()
	}
	now := s.config.now().UTC()
	if request.WorkItemID == "" && len(request.ProviderCandidates) == 0 {
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
		for _, rows := range scan.plans {
			candidate := rows.candidate
			plan, err := s.hydrateNativeClaimPlan(ctx, scope, request, now, rows, descriptor)
			if errors.Is(err, ErrNoClaimableWork) {
				continue
			}
			if err != nil {
				return leasestore.Lease{}, err
			}
			descriptor = &plan.descriptor
			if !nativeClaimIssueContractAllowed(plan) {
				s.config.Logger.InfoContext(ctx, "native claim candidate skipped",
					"organization_id", scope.organization, "project_id", scope.project,
					"work_item_id", candidate.WorkItemID, "work_item_revision", plan.item.Revision,
					"runner_id", scope.credential.Runner.RunnerID, "reason", "issue_contract_unsatisfied")
				position = claimCandidateCursor(candidate)
				continue
			}
			if (stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending) && !plan.changeReady {
				position = claimCandidateCursor(candidate)
				continue
			}
			candidateRequest := request
			candidateRequest.WorkItemID = candidate.WorkItemID
			scan.cursor.Position = position
			return s.claimNativeIssueTenantWithPlan(ctx, scope, candidateRequest, ttl, now, plan, &scan.cursor)
		}
		scan.cursor.Position = scan.pageNext
		if err := s.persistNativeClaimCursor(ctx, scope, scan.cursor); err != nil {
			return leasestore.Lease{}, err
		}
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	plan, err := s.readNativeClaimPlan(ctx, scope, request, now)
	if err != nil {
		if errors.Is(err, ErrNoClaimableWork) && explicitItem {
			if recordErr := s.recordRequestedClaimRefusal(ctx, scope, request, now); recordErr != nil {
				return leasestore.Lease{}, recordErr
			}
		}
		return leasestore.Lease{}, err
	}
	return s.claimNativeIssueTenantWithPlan(ctx, scope, request, ttl, now, plan, nil)
}

type requestedClaimRefusal struct {
	item            corestore.WorkItem
	change          tracker.NativeChangeReference
	claimedSequence int64
	eventID         string
	recorded        bool
}

func (s *Service) readRequestedClaimRefusal(ctx context.Context, r tenantstore.Reader, scope nativeScope, request tracker.NativeClaim, now time.Time) (requestedClaimRefusal, error) {
	var result requestedClaimRefusal
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return result, err
	}
	identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
	if err != nil {
		return result, err
	}
	if !runnerIdentityCanClaim(identity, scope, request.MachineID, now) {
		return result, tracker.ErrMachineNotFound
	}
	currentPolicy, err := r.ProjectPolicy(ctx, scope.project)
	if err != nil {
		return result, err
	}
	if currentPolicy.PolicyID != request.PolicyID {
		return result, policyMismatch("Claim configuration changed; load the current project policy before claiming")
	}
	result.item, err = r.WorkItem(ctx, scope.project, request.WorkItemID)
	if err != nil {
		return result, err
	}
	page, err := r.CandidatePage(ctx, nativeTenantCandidateQuery(scope, request, request.WorkItemID, now))
	if err != nil {
		return result, err
	}
	if len(page.Items) != 0 {
		return result, corestore.ErrRevisionConflict
	}
	change, err := r.LatestChange(ctx, scope.project, request.WorkItemID)
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
		return result, nativeRunEventStorageUnavailable()
	}
	claimed, err := owner.LatestClaimedSchedulerDecision(ctx, scope.project, request.WorkItemID)
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
	}{scope.organization, scope.project, request.WorkItemID, identity.RunnerID, result.item.Revision, result.change, "no_claimable_work", result.claimedSequence})
	if err != nil {
		return result, err
	}
	result.eventID = "evt_refusal_" + policy.Digest(key)
	previous, err := owner.SchedulerDecision(ctx, scope.project, request.WorkItemID, result.eventID)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	actor := scope.actor()
	if previous.SchedulerOutcome != "skipped" || previous.ActorKind != actor.Kind || previous.ActorPrincipalID != actor.PrincipalID || previous.EventRevision != result.item.Revision || previous.ChangeID != result.change.ChangeID || previous.VersionID != result.change.VersionID || previous.HeadSHA != result.change.HeadSHA {
		return result, artifact.ErrIntegrity
	}
	result.recorded = true
	return result, nil
}

func (s *Service) recordRequestedClaimRefusal(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time) error {
	var refusal requestedClaimRefusal
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	if err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		refusal, err = s.readRequestedClaimRefusal(ctx, r, scope, request, now)
		return err
	}); err != nil || refusal.recorded {
		return err
	}
	decision := tracker.CollaborationEvent{ID: refusal.eventID, OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: request.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: tracker.Revision(refusal.item.Revision), Decision: &tracker.NativeSchedulerDecision{RunnerID: scope.credential.Runner.RunnerID, Source: "native_claim_eligibility", Outcome: "skipped", Reason: "no_claimable_work", At: now, WorkItemRevision: tracker.Revision(refusal.item.Revision)}}}
	if refusal.change.ChangeID != "" {
		decision.Data.Change = &refusal.change
	}
	raw, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	row, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return err
	}
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if _, err := w.LockWorkItem(ctx, scope.project, request.WorkItemID); err != nil {
			return err
		}
		current, err := s.readRequestedClaimRefusal(ctx, w, scope, request, s.config.now().UTC())
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

func (s *Service) readNativeClaimCandidates(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time) (nativeClaimCandidateScan, error) {
	var scan nativeClaimCandidateScan
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return scan, nativeRunEventStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return ErrNoClaimableWork
		}
		policy, err := r.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if policy.PolicyID != request.PolicyID {
			return policyMismatch("Claim configuration changed; load the current project policy before claiming")
		}
		shared := nativeClaimPlan{project: project, policy: policy}
		if request.SessionID != "" {
			lease, err := r.LeaseBySession(ctx, request.SessionID)
			if err == nil {
				item, err := r.WorkItemByID(ctx, scope.project, lease.IssueID)
				if err != nil {
					return err
				}
				candidate := claimstore.Candidate{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: item.ID, Revision: item.Revision, State: valueOrEmpty(item.WorkflowStateName), CreatedAt: item.NativeCreatedAt}
				shared.replay = &lease
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
		cursor, err := r.SelectionCursor(ctx, scope.credential.Runner.RunnerID, scope.project)
		if errors.Is(err, corestore.ErrNotFound) {
			cursor = claimstore.SelectionCursor{OrganizationID: scope.organization, RunnerID: scope.credential.Runner.RunnerID, ProjectID: scope.project, SourceRevision: sourceRevision}
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
		query.Limit = nativeClaimCandidatePageSize
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
			return nativeRunEventStorageUnavailable()
		}
		rows, err := batchReader.ClaimPreviewPlanRows(ctx, scope.project, page.Items)
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
			if plan.item.Revision != plan.candidate.Revision || plan.item.Archived {
				continue
			}
			if err := validateClaimSnapshotPolicy(scope, plan); err != nil {
				return err
			}
			scan.plans = append(scan.plans, plan)
		}

		return readNativeClaimChangeFacts(ctx, r, scope, scan.plans)
	})
	if err == nil {
		plans := scan.plans
		if scan.replay != nil {
			plans = []nativeClaimPlan{*scan.replay}
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

func nativeClaimPlanFromBatch(shared nativeClaimPlan, candidate claimstore.Candidate, row tenantstore.ClaimPreviewPlanRow) nativeClaimPlan {
	plan := shared
	plan.snapshotLoaded = true
	plan.observations = row.Observations
	plan.changePolicy = currentChangePolicyRows{Project: shared.policy}
	if row.ReviewPolicy != nil {
		plan.changePolicy.Review = *row.ReviewPolicy
		plan.changePolicy.hasReview = true
	}
	plan.candidate, plan.item, plan.version = candidate, row.Item, row.Version
	if row.Item.NativeSourceKey != nil {
		plan.linkedRecord = row.LinkedSource
	}
	if row.Change != nil {
		plan.changeRecord = *row.Change
	}
	if row.ChangeVersion != nil {
		plan.changeVersion = *row.ChangeVersion
	}
	if row.ChangePolicyRevision != nil {
		plan.changePolicyRevision = *row.ChangePolicyRevision
	}
	if row.ChangeVersion != nil {
		plan.change = tracker.NativeChangeReference{ChangeID: plan.changeRecord.ID, VersionID: plan.changeVersion.ID, HeadSHA: plan.changeVersion.HeadSHA}
	}
	if row.LatestAttempt != nil {
		plan.latestAttempt, plan.latestAttemptFound = *row.LatestAttempt, true
	}
	if row.LatestRunEvent != nil {
		plan.latestRunEvent, plan.latestRunEventFound = *row.LatestRunEvent, true
	}
	plan.landingPending = nativeClaimPendingLanding(plan)
	return plan
}

func (s *Service) claimNativeIssueTenantWithPlan(ctx context.Context, scope nativeScope, request tracker.NativeClaim, ttl time.Duration, now time.Time, plan nativeClaimPlan, cursor *claimstore.SelectionCursor) (leasestore.Lease, error) {
	if !nativeClaimIssueContractAllowed(plan) {
		return leasestore.Lease{}, &nativeError{Code: "issue_contract_unsatisfied", Message: "Current issue contract does not allow dispatch", status: http.StatusConflict}
	}
	if (stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending) && !plan.changeReady {
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	if len(request.ProviderCandidates) > 0 && !slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) {
		candidate := slices.IndexFunc(request.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool {
			return candidate.WorkItemID == plan.candidate.WorkItemID
		})
		if candidate < 0 || int64(request.ProviderCandidates[candidate].Revision) != plan.candidate.Revision {
			return leasestore.Lease{}, providerWait("provider_candidate_changed", "Work item changed after local model selection; refresh before claiming")
		}
	}
	runnerAPI := true
	if plan.descriptor.Configuration != nil {
		workflow, err := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), plan.descriptor)
		if err != nil {
			return leasestore.Lease{}, err
		}
		runnerAPI = workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered
	}
	decision := tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: plan.item.NativeID, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{RunnerID: scope.credential.Runner.RunnerID, Source: "native_claim", Outcome: "claimed", Reason: "A current fenced lease owns this item", At: now, WorkItemRevision: tracker.Revision(plan.item.Revision)}}}
	decisionRaw, err := json.Marshal(decision)
	if err != nil {
		return leasestore.Lease{}, err
	}
	decisionRow, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).PrepareEvent(ctx, scope.organization, decisionRaw)
	if err != nil {
		return leasestore.Lease{}, err
	}
	var lease leasestore.Lease
	err = s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		authoritativeNow := s.config.now().UTC()
		if !runnerIdentityCanClaim(identity, scope, request.MachineID, authoritativeNow) {
			return tracker.ErrMachineNotFound
		}
		if err := plan.descriptor.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
			return policyMismatch(err.Error())
		}
		if identity.State != "active" || identity.CapacityConfig == nil || !authoritativeNow.Before(identity.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout)) || authoritativeNow.Before(identity.CapacityConfig.ObservedAt) {
			return ErrNoClaimableWork
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, authoritativeNow)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNoClaimableWork
		}
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if request.LandingCapacity > 0 || plan.landingRequired {
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return ErrNoClaimableWork
		}
		item, err := w.WorkItemByID(ctx, scope.project, plan.candidate.ID)
		if err != nil {
			return err
		}
		if item.NativeID != plan.candidate.WorkItemID || item.Archived || plan.replay == nil && (item.Revision != plan.candidate.Revision || item.DispatchGeneration != plan.item.DispatchGeneration) {
			return ErrNoClaimableWork
		}
		currentLinked, err := w.LinkedSource(ctx, scope.project, item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && (plan.linkedRecord == nil || !sameNativeClaimLinkedSource(currentLinked, *plan.linkedRecord)) || errors.Is(err, corestore.ErrNotFound) && plan.linkedRecord != nil {
			return corestore.ErrRevisionConflict
		}
		workspaceItem, err := w.IsWorkspaceWorkItem(ctx, scope.project, item.NativeID)
		if err != nil {
			return err
		}
		workspaceLane := slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability)
		if workspaceItem && !workspaceLane || !workspaceItem && slices.Contains(request.Capabilities, tracker.NativeProviderCapacityCapability) && len(request.ProviderCandidates) == 0 {
			return ErrNoClaimableWork
		}
		item, err = w.LockWorkItem(ctx, scope.project, item.NativeID)
		if err != nil {
			return err
		}
		if item.Archived || plan.replay == nil && (item.Revision != plan.candidate.Revision || item.DispatchGeneration != plan.item.DispatchGeneration) {
			return corestore.ErrRevisionConflict
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
		replay := plan.replay
		if replay == nil && replayErr == nil && replayOwner.RunnerID == identity.RunnerID {
			replay = &replayLease
		}
		if replay == nil {
			query := nativeTenantCandidateQuery(scope, request, plan.candidate.WorkItemID, authoritativeNow)
			page, err := w.CandidatePage(ctx, query)
			if err != nil {
				return err
			}
			if len(page.Items) != 1 || page.Items[0].Revision != plan.candidate.Revision {
				return ErrNoClaimableWork
			}
			latest, err := w.LatestAttempt(ctx, item.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			current := plan
			current.item = item
			current.latestAttempt, current.latestAttemptFound = latest, err == nil
			if nativeClaimAnswerRequired(current) && current.latestAttempt.ClaimDisposition == nil {
				return nativeRunEventStorageUnavailable()
			}

			if nativeClaimDispatchSatisfied(current) {
				return ErrNoClaimableWork
			}
		}
		if stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending {
			latest, err := w.LatestChange(ctx, scope.project, plan.candidate.WorkItemID)
			if err != nil || latest.ID != plan.change.ChangeID || latest.CurrentVersionID == nil || *latest.CurrentVersionID != plan.change.VersionID {
				if err != nil {
					return err
				}
				return corestore.ErrRevisionConflict
			}
			current, err := w.LockChange(ctx, plan.changeRecord.ID)
			if err != nil {
				return err
			}
			if !sameNativeClaimChangeRecord(current, plan.changeRecord) {
				return corestore.ErrRevisionConflict
			}
			version, err := w.Version(ctx, current.ID, plan.change.VersionID)
			if err != nil {
				return err
			}
			if version.PolicyRevisionID != plan.changeVersion.PolicyRevisionID || version.HeadSHA != plan.changeVersion.HeadSHA {
				return corestore.ErrRevisionConflict
			}
			versionPolicy, err := w.ProjectPolicyRevision(ctx, scope.project, version.PolicyRevisionID)
			if err != nil {
				return err
			}
			if !sameNativeClaimPolicyRevision(versionPolicy, plan.changePolicyRevision) {
				return corestore.ErrRevisionConflict
			}
			currentPolicy, err := readCurrentChangePolicyRows(ctx, w, scope)
			if err != nil {
				return err
			}
			if !sameNativeClaimChangePolicyRows(currentPolicy, plan.changePolicy) {
				return corestore.ErrRevisionConflict
			}
			if err := verifyNativeClaimLandingEvent(ctx, w, item.ID, plan); err != nil {
				return err
			}
		}
		if replay == nil && (plan.landingRequired || request.LandingCapacity > 0) {
			usage, err := w.LandingCapacityUsage(ctx, scope.project, authoritativeNow)
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
		}
		exposure := budget.CostExposure{RunnerAPI: !workspaceItem && runnerAPI}
		if replay == nil {
			capacity := min(identity.CapacityLimit, identity.ReportedCapacity, int64(identity.CapacityConfig.LocalLimit), int64(identity.CapacityConfig.ClientLimit), int64(identity.CapacityConfig.RuntimeLimit))
			projectCapacity, projectCapacityConfigured := identity.ProjectCapacityLimits[scope.project]
			if projectCapacityConfigured {
				capacity = min(capacity, int64(projectCapacity))
			}
			availability := runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: identity.Availability.Windows, WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline}
			availableCapacity, err := availability.Capacity(authoritativeNow, int(capacity))
			if err != nil {
				return err
			}
			if availableCapacity <= 0 {
				return ErrNoClaimableWork
			}
			machine, err := w.LockMachine(ctx, string(identity.MachineID))
			if err != nil {
				return err
			}
			if err := runnerVersionError(minimumRunnerVersion(s.config.Version), machine.Version); err != nil {
				return err
			}
			exposure.SpriteInfrastructure = machine.SpriteName != "" && machine.SpriteName == machine.Hostname
			batch := ""
			if stringsEqualFold(plan.candidate.State, "Merging") && plan.changeReady && !plan.landingRequired {
				parts := strings.Split(request.SessionID, "/")
				if len(parts) == 3 && parts[0] == "landing" && parts[1] != "" && parts[2] != "" {
					batch = parts[1]
				}
			}
			usage, err := w.ClaimCapacityUsage(ctx, machine.ID, identity.RunnerID, scope.project, batch, authoritativeNow)
			if err != nil {
				return err
			}
			if machine.Capacity <= 0 || usage.Host >= machine.Capacity {
				return ErrNoClaimableWork
			}
			if usage.Runner >= int64(availableCapacity) || projectCapacityConfigured && usage.Project >= int64(projectCapacity) {
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
				exposure.SpriteInfrastructure = machine.SpriteName != "" && machine.SpriteName == machine.Hostname
			} else {
				return err
			}
		}
		var selectedReservation *providercapacity.Reservation
		if !workspaceItem && replay == nil && len(request.ProviderCandidates) > 0 {
			reservation, err := selectTenantProviderReservation(ctx, w, identity, request, item.NativeID, authoritativeNow)
			if err != nil {
				return err
			}
			selectedReservation = &reservation
		}
		leaseID := newNativeID("lease")
		lease, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(identity.MachineID), SessionID: request.SessionID, LeaseID: leaseID, TTL: ttl, LandingCapacity: int64(request.LandingCapacity)})
		if err != nil {
			return err
		}
		currentPolicy, err := w.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if !sameNativeClaimPolicyRevision(currentPolicy, plan.policy) || currentPolicy.ApprovedBy != plan.policy.ApprovedBy || !currentPolicy.ApprovedAt.Equal(plan.policy.ApprovedAt) {
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
			if pinned.PolicyID != plan.policy.PolicyID {
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
			if err := w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: scope.organization, LeaseID: lease.LeaseID, RunnerID: identity.RunnerID, AssignedAt: authoritativeNow}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(ctx, policystore.LeasePolicy{OrganizationID: scope.organization, ProjectID: scope.project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: plan.policy.PolicyID, BoundAt: authoritativeNow}); err != nil {
				return err
			}
			if selectedReservation != nil {
				row := claimstore.ProviderReservation{OrganizationID: scope.organization, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: *selectedReservation}
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
			return &nativeError{Code: "provider_capacity", Message: monthlyBudgetRefusal(decision), status: http.StatusConflict, Details: map[string]any{"monthly_budget": decision}}
		}
		if lease.LeaseID == leaseID {
			if err := w.RecordBillingMonthlyAdmission(ctx, billingstore.MonthlyAdmission{OrganizationID: scope.organization, IssueID: tracker.WorkItemID(item.ID), AdmittedAt: authoritativeNow}); err != nil {
				return err
			}
			if err := w.RecordBillingMonthlyLease(ctx, billingstore.MonthlyLease{OrganizationID: scope.organization, LeaseID: tracker.LeaseID(lease.LeaseID), LunaAPI: exposure.LunaAPI, RunnerAPI: exposure.RunnerAPI, SpriteInfrastructure: exposure.SpriteInfrastructure}); err != nil {
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
	})
	return lease, err
}

func (s *Service) writeNativeClaimCommand(ctx context.Context, scope nativeScope, write func(context.Context, tenantstore.Writer) error) error {
	if command, ok := s.config.TenantRepository.(interface {
		WriteRunnerCommand(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	}); ok {
		return command.WriteRunnerCommand(ctx, scope.organization, scope.credential.ID, scope.credential.Runner.RunnerID, write)
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
}

func (s *Service) persistNativeClaimCursor(ctx context.Context, scope nativeScope, cursor claimstore.SelectionCursor) error {
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		authoritativeNow := s.config.now().UTC()
		if !runnerIdentityCanClaim(identity, scope, scope.credential.Runner.MachineID, authoritativeNow) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, authoritativeNow)
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
	return nativeCommandHash(struct {
		WorkflowSourceRevision string
		PolicyID               string
		PolicySHA256           string
		Query                  claimstore.CandidateQuery
	}{project.Core.WorkflowSourceRevision, policy.PolicyID, policy.PayloadSHA256, query})
}

func selectTenantProviderReservation(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, request tracker.NativeClaim, item tracker.NativeWorkItemID, now time.Time) (providercapacity.Reservation, error) {
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
	used, err := w.ProviderReservationUsage(ctx, *selected, identity.RunnerID, now)
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

func (s *Service) readNativeClaimPlan(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time) (nativeClaimPlan, error) {
	plan, err := s.readNativeClaimPlanRows(ctx, scope, request, now, nil, nil, nativeClaimPlan{})
	if err != nil {
		return nativeClaimPlan{}, err
	}
	if err := s.prepareNativeClaimLegacyAuditFacts(ctx, scope, []nativeClaimPlan{plan}); err != nil {
		return nativeClaimPlan{}, err
	}
	plans := []nativeClaimPlan{plan}
	if err := s.prepareNativeClaimApprovals(ctx, scope, plans); err != nil {
		return nativeClaimPlan{}, err
	}
	return s.hydrateNativeClaimPlan(ctx, scope, request, now, plans[0], nil)
}

func (s *Service) readNativeClaimPlanRows(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time, reader tenantstore.Reader, candidate *claimstore.Candidate, shared nativeClaimPlan) (nativeClaimPlan, error) {
	plan := shared
	readRows := func(r tenantstore.Reader) error {
		var err error
		if plan.project.Core.ID == "" {
			plan.project, err = r.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if plan.project.Core.Profile != "native" || plan.project.Core.DeletedAt != nil {
				return ErrNoClaimableWork
			}
			plan.policy, err = r.ProjectPolicy(ctx, scope.project)
			if err != nil {
				return err
			}
			if plan.policy.PolicyID != request.PolicyID {
				return policyMismatch("Claim configuration changed; load the current project policy before claiming")
			}
		}
		if request.SessionID != "" && plan.replay == nil {
			lease, err := r.LeaseBySession(ctx, request.SessionID)
			if err == nil {
				plan.replay = &lease
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if plan.replay != nil {
			lease := plan.replay
			if lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.MachineID != string(request.MachineID) {
				return tracker.ErrLeaseConflict
			}
			if candidate == nil {
				item, err := r.WorkItemByID(ctx, scope.project, lease.IssueID)
				if err != nil {
					return err
				}
				value := claimstore.Candidate{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: item.ID, Revision: item.Revision, State: valueOrEmpty(item.WorkflowStateName), CreatedAt: item.NativeCreatedAt}
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
		batch, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		rows, err := batch.ClaimPreviewPlanRows(ctx, scope.project, []claimstore.Candidate{*candidate})
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
		plans := []nativeClaimPlan{plan}
		if err := readNativeClaimChangeFacts(ctx, r, scope, plans); err != nil {
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
			return plan, nativeRunEventStorageUnavailable()
		}
		err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			return readRows(r)
		})
	}
	return plan, err
}

func (s *Service) prepareNativeClaimApprovals(ctx context.Context, scope nativeScope, plans []nativeClaimPlan) error {
	var approval *ChangeVersionApproval
	for i := range plans {
		if (stringsEqualFold(plans[i].candidate.State, "Merging") || plans[i].landingPending) && plans[i].changeRecord.ID != "" {
			if approval == nil {
				value, err := hydrateChangeSummaryPolicy(ctx, eventstore.NewContent(s.runDetailStoreFor(ctx)), scope, plans[i].changePolicy)
				if err != nil {
					return err
				}
				approval = &value
			}
			plans[i].approval = approval
		}
	}
	return nil
}

func validateClaimSnapshotPolicy(scope nativeScope, plan nativeClaimPlan) error {
	project := plan.changePolicy.Project
	if project.OrganizationID != scope.organization || project.ProjectID != scope.project || project.PolicyID == "" || project.ApprovedBy == "" || project.ApprovedAt.IsZero() {
		return corestore.ErrTenantMismatch
	}
	if plan.changePolicy.hasReview {
		review := plan.changePolicy.Review
		if review.OrganizationID != scope.organization || review.ProjectID != scope.project || review.PolicyRevisionID == "" || review.ReviewPolicyID == "" || review.Revision < 1 || review.UpdatedBy == "" || review.UpdatedAt.IsZero() {
			return corestore.ErrTenantMismatch
		}
	}
	return nil
}

func (s *Service) hydrateNativeClaimPlan(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time, plan nativeClaimPlan, descriptor *policy.Descriptor) (nativeClaimPlan, error) {
	if !plan.snapshotLoaded {
		return plan, nativeRunEventStorageUnavailable()
	}
	if nativeClaimAnswerRequired(plan) && plan.latestAttempt.ClaimDisposition == nil {
		return nativeClaimPlan{}, nativeRunEventStorageUnavailable()
	}
	var err error
	if descriptor == nil && plan.approval != nil {
		descriptor = &plan.approval.Policy
	}
	if descriptor == nil {
		loaded, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, plan.policy)
		if err != nil {
			return nativeClaimPlan{}, err
		}
		descriptor = &loaded
	}
	plan.descriptor = *descriptor
	if plan.descriptor.ID != request.PolicyID {
		return nativeClaimPlan{}, policyMismatch("Claim configuration does not match the current project policy")
	}
	if plan.policy.GitHubPullRequest == nil || *plan.policy.GitHubPullRequest != plan.descriptor.Gates.GitHubPullRequest {
		return nativeClaimPlan{}, ErrNoClaimableWork
	}
	raw, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).CollaborationVersion(ctx, plan.version)
	if err != nil {
		return nativeClaimPlan{}, err
	}
	if err := json.Unmarshal(raw, &plan.issue); err != nil || plan.issue.OrganizationID != plan.item.OrganizationID || plan.issue.ProjectID != plan.item.ProjectID || plan.issue.WorkItemID != plan.item.NativeID || int64(plan.issue.Revision) != plan.item.Revision || int64(plan.issue.Number) != plan.item.Number || plan.issue.Title != plan.item.Title || plan.issue.Archived != plan.item.Archived || plan.issue.Actor.Kind != valueOrEmpty(plan.item.ActorKind) || plan.issue.Actor.PrincipalID != valueOrEmpty(plan.item.ActorPrincipalID) {
		return nativeClaimPlan{}, corestore.ErrRevisionConflict
	}

	if nativeClaimDispatchSatisfied(plan) {
		return nativeClaimPlan{}, ErrNoClaimableWork
	}
	plan.issue.State = valueOrEmpty(plan.item.WorkflowStateName)
	plan.issue.LastActivityAt = plan.item.LastActivityAt
	if plan.linkedRecord != nil {
		record := plan.linkedRecord
		linked := tracker.LinkedIssueSource{URL: record.URL, Status: "pending"}
		if record.SnapshotObjectKey != "" {
			snapshot, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).LinkedIssueSnapshot(ctx, scope.organization, eventstore.Reference(record.SnapshotObjectKey, record.SnapshotSHA256, record.SnapshotBytes, record.SnapshotContentBytes))
			if err != nil {
				return nativeClaimPlan{}, err
			}
			if snapshot.URL != record.URL {
				return nativeClaimPlan{}, artifact.ErrIntegrity
			}
			linked.Status, linked.Snapshot = "complete", &snapshot
		}
		plan.linked = &linked
		plan.issue.LinkedSource = &linked
	}
	if stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending {
		if plan.changeRecord.ID == "" || plan.changeRecord.CurrentVersionID == nil || *plan.changeRecord.CurrentVersionID != plan.changeVersion.ID || plan.changeVersion.ID != plan.change.VersionID || plan.changeVersion.HeadSHA != plan.change.HeadSHA {
			return plan, nil
		}
		detail, err := s.nativeClaimCurrentChangeDetail(ctx, scope, plan, now)
		if err != nil {
			return nativeClaimPlan{}, err
		}
		state := changerequest.ReviewState(detail, plan.changeVersion.ID, plan.changeVersion.HeadSHA)
		currentVersion := valueOrEmpty(plan.changeRecord.CurrentVersionID)
		plan.changeReady = plan.changeRecord.OrganizationID == scope.organization && plan.changeRecord.ProjectID == scope.project && plan.changeRecord.WorkItemID == plan.candidate.WorkItemID && detail.Change.ID == plan.changeRecord.ID && int64(detail.Change.Revision) == plan.changeRecord.Revision && detail.Change.CurrentVersion == currentVersion && currentVersion == plan.changeVersion.ID && state.Current && !state.ChangesRequested && !state.RepairPending && nativeChangeLandingClaimable("Merging", &detail)
	}
	plan.landingRequired, err = nativeClaimLandingCapacityRequired(plan)
	if err != nil {
		return nativeClaimPlan{}, err
	}
	return plan, nil
}

func nativeClaimLandingCapacityRequired(plan nativeClaimPlan) (bool, error) {
	if stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending {
		if plan.changePolicyRevision.GitHubPullRequest == nil {
			return false, ErrNoClaimableWork
		}
		return *plan.changePolicyRevision.GitHubPullRequest, nil
	}
	return false, nil
}

func nativeClaimPendingLanding(plan nativeClaimPlan) bool {
	if stringsEqualFold(plan.candidate.State, "Rework") || !plan.latestAttemptFound || !plan.latestRunEventFound || !plan.latestRunEvent.LandingWaiting || plan.changeRecord.ID == "" || plan.changeRecord.CurrentVersionID == nil {
		return false
	}
	return plan.latestRunEvent.LandingChangeID == plan.changeRecord.ID && plan.latestRunEvent.LandingVersionID == plan.changeVersion.ID && plan.latestRunEvent.LandingHeadSHA == plan.changeVersion.HeadSHA && plan.changeVersion.ID == *plan.changeRecord.CurrentVersionID
}

func verifyNativeClaimLandingEvent(ctx context.Context, r tenantstore.Reader, issueID int64, plan nativeClaimPlan) error {
	attempt, err := r.LatestAttempt(ctx, issueID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		if plan.latestAttemptFound {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !plan.latestAttemptFound || attempt.ID != plan.latestAttempt.ID || attempt.FencingToken != plan.latestAttempt.FencingToken || attempt.Sequence != plan.latestAttempt.Sequence {
		return corestore.ErrRevisionConflict
	}
	event, err := r.LatestRunEvent(ctx, plan.candidate.ProjectID, attempt.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		if plan.latestRunEventFound {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !plan.latestRunEventFound || event.ID != plan.latestRunEvent.ID || event.Sequence != plan.latestRunEvent.Sequence || event.LandingWaiting != plan.latestRunEvent.LandingWaiting || event.LandingChangeID != plan.latestRunEvent.LandingChangeID || event.LandingVersionID != plan.latestRunEvent.LandingVersionID || event.LandingHeadSHA != plan.latestRunEvent.LandingHeadSHA {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func sameNativeClaimChangeRecord(left, right eventstore.Change) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.WorkItemID == right.WorkItemID && left.ID == right.ID && left.Revision == right.Revision && left.Title == right.Title && valueOrEmpty(left.CurrentVersionID) == valueOrEmpty(right.CurrentVersionID) && left.EvidenceSequence == right.EvidenceSequence && left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimChangePolicyRows(left, right currentChangePolicyRows) bool {
	return sameNativeClaimProjectPolicy(left.Project, right.Project) && sameNativeClaimReviewPolicy(left.Review, right.Review) && left.hasReview == right.hasReview
}

func sameNativeClaimProjectPolicy(left, right policystore.Revision) bool {
	return sameNativeClaimPolicyRevision(left, right) && left.ApprovedBy == right.ApprovedBy && left.ApprovedAt.Equal(right.ApprovedAt)
}

func sameNativeClaimPolicyRevision(left, right policystore.Revision) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && sameNativeClaimOptionalBool(left.GitHubPullRequest, right.GitHubPullRequest) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimOptionalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameNativeClaimReviewPolicy(left eventstore.ReviewPolicy, right eventstore.ReviewPolicy) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyRevisionID == right.PolicyRevisionID && left.ReviewPolicyID == right.ReviewPolicyID && left.Revision == right.Revision && left.UpdatedBy == right.UpdatedBy && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimLinkedSource(left, right eventstore.LinkedSource) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.WorkItemID == right.WorkItemID && left.URL == right.URL && left.TitleSupplied == right.TitleSupplied && left.BodySupplied == right.BodySupplied && left.TitleOwned == right.TitleOwned && left.BodyOwned == right.BodyOwned && left.SnapshotObjectKey == right.SnapshotObjectKey && left.SnapshotSHA256 == right.SnapshotSHA256 && left.SnapshotBytes == right.SnapshotBytes && left.SnapshotContentBytes == right.SnapshotContentBytes && stringValue(left.ExpectedSnapshotSHA256) == stringValue(right.ExpectedSnapshotSHA256)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nativeClaimIssueContractAllowed(plan nativeClaimPlan) bool {
	if strings.Contains(strings.ToLower(plan.issue.Body), "detent:no-dispatch") {
		return false
	}
	if plan.issue.LinkedSource != nil && plan.issue.LinkedSource.Status != "complete" || strings.EqualFold(plan.issue.State, "Triage") || plan.issue.IssueContract != nil && plan.issue.IssueContract.Exempt {
		return true
	}
	contract, err := config.ResolvePolicyIssueContract(plan.descriptor)
	if err != nil {
		return false
	}
	target := "Todo"
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, plan.descriptor)
	if err != nil {
		return false
	}
	if workflow.Config.BacklogAdmission.TargetState != "" {
		target = workflow.Config.BacklogAdmission.TargetState
	}
	if !strings.EqualFold(plan.issue.State, target) {
		return true
	}
	if plan.issue.IssueContract != nil && plan.issue.IssueContract.ReleaseDelivery != nil {
		return false
	}
	evaluation := contract.Evaluate(connector.Issue{Title: plan.issue.Title, Description: plan.issue.Body, IssueContract: plan.issue.IssueContract})
	return evaluation.Satisfied()
}

func nativeTenantCandidateQuery(scope nativeScope, request tracker.NativeClaim, item tracker.NativeWorkItemID, now time.Time) claimstore.CandidateQuery {
	workItemIDs := make([]tracker.NativeWorkItemID, 0, len(request.ProviderCandidates))
	for _, candidate := range request.ProviderCandidates {
		workItemIDs = append(workItemIDs, candidate.WorkItemID)
	}
	return claimstore.CandidateQuery{
		ProjectID: scope.project, Scope: string(scope.project), WorkItemID: item, WorkItemIDs: workItemIDs,
		States: request.WorkflowStates, Authors: request.Authors, Assignees: request.Assignees,
		IncludeLabels: request.LabelInclude, ExcludeLabels: request.LabelExclude,
		WorkspaceLane:   slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability),
		RunnerSelection: true, PrioritizeUnblockers: request.PrioritizeUnblockers,
		StatePriority: request.DispatchPriorityByState, LabelPriority: request.DispatchPriorityByLabel,
		AvailableAt: now, Limit: 1,
	}
}

func runnerIdentityCanClaim(identity runnerstore.Identity, scope nativeScope, machine tracker.MachineID, now time.Time) bool {
	return identity.OrganizationID == scope.organization && identity.TokenID == scope.credential.ID && identity.RunnerID == scope.credential.Runner.RunnerID && identity.MachineID == machine && machine == scope.credential.Runner.MachineID && identity.State == "active" && identity.RemovedAt == nil && identity.ExpiresAt.After(now) && !now.Before(identity.LastHeartbeatAt) && now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) && slices.Contains(identity.Operations, runnerauth.Claim)
}

func stringsEqualFold(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}
