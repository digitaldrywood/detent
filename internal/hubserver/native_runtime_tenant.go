package hubserver

import (
	"context"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeRuntimeTenantSnapshot struct {
	project              projectstore.Record
	item                 corestore.WorkItem
	version              eventstore.CollaborationVersion
	attempt              hubrecords.TenantAttemptSnapshot
	currentLease         *leasestore.Lease
	attemptPolicy        policystore.Revision
	currentPolicy        policystore.Revision
	policyFound          bool
	leasePolicy          policystore.LeasePolicy
	currentPolicyBinding *policystore.LeasePolicy
	transition           eventstore.Event
	decision             eventstore.Event
	changeRecord         eventstore.Change
	changeVersion        *eventstore.Version
	changePolicy         hubrecords.CurrentChangePolicyRows
	boardSequence        int64
	reservation          *claimstore.ProviderReservation
}

func (s *Service) readNativeRuntimeTenant(ctx context.Context, scope hubscope.NativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	if len(admission) > 1 {
		return tracker.NativeRuntimeEvidence{}, hubapi.NativeRunEventStorageUnavailable()
	}
	if attemptID == "" {
		row, version, err := s.readWorkItemRecord(ctx, scope, item)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
		issue, err := s.hydrateWorkItemRecord(ctx, row, version)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
		if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, &issue); err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
		evidence, _, _, err := s.readOperatorEvidence(ctx, scope, issue, false)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
		if err := s.readRuntimeAdmissionTenant(ctx, scope, issue, &evidence, admission); err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
		if evidence.Attempt == nil {
			if err := s.readRuntimeSourceRecoveryTenant(ctx, scope, item, &evidence); err != nil {
				return evidence, err
			}
		}
		return evidence, nil
	}
	evidence, _, _, err := s.readSelectedNativeRuntimeTenant(ctx, scope, item, runtimeAttemptSelector{attemptID: attemptID})
	return evidence, err
}

type runtimeAttemptSelector struct {
	attemptID string
	latest    bool
	failures  *runtimeFailureRows
}

type runtimeFailureRows struct {
	rows   []leasestore.AttemptFailureRow
	legacy map[string]hubrecords.TenantAttemptSnapshot
}

const runtimeFailureHistory = 64

func readRuntimeFailureRows(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, row corestore.WorkItem, out *runtimeFailureRows) error {
	reader, ok := r.(leasestore.AttemptFailureReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	rows, err := reader.AttemptFailures(ctx, row.ID, runtimeFailureHistory+1)
	if err != nil {
		return err
	}
	out.rows, out.legacy = rows, map[string]hubrecords.TenantAttemptSnapshot{}
	for _, failure := range rows[:min(len(rows), runtimeFailureHistory)] {
		if failure.Recorded {
			continue
		}
		attempt, err := r.Attempt(ctx, failure.AttemptID)
		if err != nil {
			return err
		}
		if attempt.OrganizationID != scope.Organization || attempt.IssueID != row.ID {
			return corestore.ErrNotFound
		}
		lease, err := r.Lease(ctx, attempt.LeaseID)
		if err != nil {
			return err
		}
		if lease.OrganizationID != scope.Organization || lease.IssueID != row.ID || lease.FencingToken != attempt.FencingToken {
			return corestore.ErrNotFound
		}
		event, err := r.LatestRunEvent(ctx, scope.Project, attempt.ID)
		if err != nil {
			return err
		}
		if event.OrganizationID != scope.Organization || event.ProjectID != scope.Project || event.WorkItemID != row.NativeID || event.AttemptID != attempt.ID {
			return corestore.ErrNotFound
		}
		snapshot := hubrecords.TenantAttemptSnapshot{Attempt: attempt, Lease: lease, Event: event}
		if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot); err != nil {
			return err
		}
		out.legacy[attempt.ID] = snapshot
	}
	return nil
}

func (s *Service) readSelectedNativeRuntimeTenant(ctx context.Context, scope hubscope.NativeScope, item string, selector runtimeAttemptSelector) (tracker.NativeRuntimeEvidence, int64, bool, error) {
	attemptID := selector.attemptID
	var localAttemptID int64
	if !selector.latest && !hubapi.ValidID(attemptID, "attempt") {
		local, err := strconv.ParseInt(attemptID, 10, 64)
		if err != nil || local <= 0 {
			return tracker.NativeRuntimeEvidence{}, 0, false, hubapi.NativePublicInvalid("Invalid attempt identity")
		}
		localAttemptID = local
	}
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok || s.runDetailStoreFor(ctx) == nil {
		return tracker.NativeRuntimeEvidence{}, 0, false, hubapi.NativeRunEventStorageUnavailable()
	}
	var snapshot nativeRuntimeTenantSnapshot
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{Kind: hubapi.StringValue(row.ActorKind), PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		version, err := r.CollaborationVersion(ctx, item, row.Revision)
		if err != nil {
			return err
		}
		board, err := r.BoardSequence(ctx, scope.Project)
		if err != nil {
			return err
		}
		snapshot.project, snapshot.item, snapshot.version, snapshot.boardSequence = project, row, version, board.Sequence
		var attempt leasestore.Attempt
		if selector.latest {
			attempt, err = r.LatestAttempt(ctx, row.ID)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
		} else {
			attempt, err = selectedRuntimeAttempt(ctx, r, scope, row, attemptID, localAttemptID)
		}
		if err != nil {
			return err
		}
		if attempt.OrganizationID != scope.Organization || attempt.IssueID != row.ID {
			return hubapi.NativeNotFound()
		}
		if selector.failures != nil {
			if err := readRuntimeFailureRows(ctx, r, scope, row, selector.failures); err != nil {
				return err
			}
		}
		lease, err := r.Lease(ctx, attempt.LeaseID)
		if err != nil {
			return err
		}
		if lease.OrganizationID != scope.Organization || lease.IssueID != row.ID || lease.FencingToken != attempt.FencingToken {
			return hubapi.NativeNotFound()
		}
		event, err := r.LatestRunEvent(ctx, scope.Project, attempt.ID)
		if err != nil {
			return err
		}
		if event.OrganizationID != scope.Organization || event.ProjectID != scope.Project || event.WorkItemID != row.NativeID || event.AttemptID != attempt.ID {
			return corestore.ErrNotFound
		}
		leasePolicy, err := r.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
		if err != nil {
			return err
		}
		if leasePolicy.OrganizationID != scope.Organization || leasePolicy.ProjectID != scope.Project || leasePolicy.LeaseID != tracker.LeaseID(lease.LeaseID) || leasePolicy.PolicyID == "" {
			return hubapi.PolicyMismatch("Lease policy is missing or outside the authorized project")
		}
		attemptPolicy, err := r.ProjectPolicyRevision(ctx, scope.Project, leasePolicy.PolicyID)
		if err != nil {
			return err
		}
		currentPolicy, policyErr := r.ProjectPolicy(ctx, scope.Project)
		policyFound := policyErr == nil
		if policyErr != nil && !errors.Is(policyErr, corestore.ErrNotFound) {
			return policyErr
		}
		change, changeErr := r.LatestChange(ctx, scope.Project, row.NativeID)
		if changeErr != nil && !errors.Is(changeErr, corestore.ErrNotFound) {
			return changeErr
		}
		if changeErr == nil {
			snapshot.changeRecord = change
			if change.CurrentVersionID != nil {
				current, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
				if err != nil {
					return err
				}
				snapshot.changeVersion = &current
			}
			snapshot.changePolicy, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
			if err != nil {
				return err
			}
		}
		if history, ok := r.(eventstore.WorkflowTransitionReader); ok {
			transition, err := history.LatestWorkflowTransition(ctx, scope.Project, row.NativeID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				snapshot.transition = transition
			}
			decision, err := history.LatestSchedulerDecision(ctx, scope.Project, row.NativeID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				snapshot.decision = decision
			}
		}
		snapshot.attempt = hubrecords.TenantAttemptSnapshot{Attempt: attempt, Lease: lease, Event: event}
		if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot.attempt); err != nil {
			return err
		}
		if err := readTenantAttemptEvidence(ctx, r, &snapshot.attempt); err != nil {
			return err
		}
		snapshot.attemptPolicy, snapshot.currentPolicy, snapshot.policyFound = attemptPolicy, currentPolicy, policyFound
		snapshot.leasePolicy = leasePolicy
		activeLease, activeErr := r.ActiveLeaseForIssue(ctx, row.ID)
		if activeErr == nil {
			if activeLease.OrganizationID != scope.Organization || activeLease.IssueID != row.ID {
				return corestore.ErrNotFound
			}
			snapshot.currentLease = &activeLease
			currentBinding, bindingErr := r.LeasePolicy(ctx, tracker.LeaseID(activeLease.LeaseID))
			if bindingErr != nil {
				return bindingErr
			}
			if currentBinding.OrganizationID != scope.Organization || currentBinding.ProjectID != scope.Project || currentBinding.LeaseID != tracker.LeaseID(activeLease.LeaseID) || currentBinding.PolicyID == "" {
				return hubapi.PolicyMismatch("Current lease policy is missing or outside the authorized project")
			}
			snapshot.currentPolicyBinding = &currentBinding
			reservation, reservationErr := r.ProviderReservation(ctx, tracker.LeaseID(activeLease.LeaseID))
			if reservationErr == nil {
				snapshot.reservation = &reservation
			} else if !errors.Is(reservationErr, corestore.ErrNotFound) {
				return reservationErr
			}
		} else if !errors.Is(activeErr, corestore.ErrNotFound) {
			return activeErr
		}
		return nil
	})
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, 0, false, hubapi.ProjectRecordError(err)
	}
	if snapshot.attempt.Attempt.ID == "" {
		return tracker.NativeRuntimeEvidence{}, snapshot.boardSequence, false, nil
	}
	issue, err := s.hydrateWorkItemRecord(ctx, snapshot.item, snapshot.version)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, 0, false, err
	}
	if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, &issue); err != nil {
		return tracker.NativeRuntimeEvidence{}, 0, false, err
	}
	issue.ChangeSequence = snapshot.boardSequence
	issue = issue.RuntimeReference()
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot.attempt, s.config.Clock().UTC())
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, 0, false, err
	}
	if attempt.PolicyID == "" || attempt.PolicyID != snapshot.leasePolicy.PolicyID {
		return tracker.NativeRuntimeEvidence{}, 0, false, hubapi.PolicyMismatch("Attempt policy does not match its fenced lease")
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	boundPolicy, err := content.ProjectPolicy(ctx, snapshot.attemptPolicy)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, 0, false, err
	}
	if err := boundPolicy.Validate(); err != nil || boundPolicy.ID != attempt.PolicyID {
		return tracker.NativeRuntimeEvidence{}, 0, false, hubapi.PolicyMismatch("Attempt policy reference is invalid")
	}
	if snapshot.policyFound {
		currentPolicy, err := content.ProjectPolicy(ctx, snapshot.currentPolicy)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, 0, false, err
		}
		if err := currentPolicy.Validate(); err != nil {
			return tracker.NativeRuntimeEvidence{}, 0, false, err
		}
	}
	now := s.config.Clock().UTC()
	attempt.Current = snapshot.attempt.Lease.ReleasedAt == nil && !now.Before(snapshot.attempt.Lease.RenewedAt) && now.Before(snapshot.attempt.Lease.ExpiresAt)
	if attempt.Runtime != nil {
		attempt.RuntimeFreshness = "available"
		heartbeat := attempt.Runtime.HeartbeatAt
		if heartbeat.IsZero() || now.Before(heartbeat) || snapshot.attempt.Attempt.Status == "running" && (!attempt.Current || now.Sub(heartbeat) >= snapshot.attempt.Lease.ExpiresAt.Sub(snapshot.attempt.Lease.RenewedAt)) {
			attempt.RuntimeFreshness = "expired"
		}
	}
	attempt.Status = snapshot.attempt.Attempt.Status
	if attempt.Status == "running" && !attempt.Current {
		attempt.Status = "interrupted"
	}
	result := tracker.NativeRuntimeEvidence{
		Issue: issue, Attempt: &attempt, ObservedAt: now, Selection: "selected",
		Unavailable: []string{"efficiency_receipt", "source_recovery_projection", "scheduler_history_projection", "change_gate_projection", "runner_capacity_projection", "native_admission_projection"},
		Capacity:    []tracker.NativeRuntimeCapacity{},
		Scheduling:  tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "unknown", At: now, WorkItemRevision: issue.Revision, Reason: "Runner capacity and admission projection are not available from this typed read"},
	}
	if snapshot.transition.ID != "" {
		transition, err := content.Event(ctx, snapshot.transition)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, 0, false, err
		}
		result.LatestTransition = &transition
	}
	if snapshot.decision.ID != "" {
		decision, err := content.Event(ctx, snapshot.decision)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, 0, false, err
		}
		result.LatestDecision = &decision
	}
	if snapshot.changeRecord.ID != "" {
		detail, err := s.hydrateCurrentChangeDetail(ctx, scope, issue.WorkItemID, snapshot.changeRecord, snapshot.changeVersion, snapshot.changePolicy, now)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, 0, false, err
		}
		attachRuntimeChangeDetail(&result, detail)
	}
	if !snapshot.policyFound {
		result.Unavailable = append(result.Unavailable, "approved_dispatch_policy")
	}
	if snapshot.currentLease != nil && !now.Before(snapshot.currentLease.RenewedAt) && now.Before(snapshot.currentLease.ExpiresAt) {
		lease := snapshot.currentLease
		current := tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt, PolicyID: snapshot.currentPolicyBinding.PolicyID, ServerTime: now}
		if snapshot.reservation != nil {
			reservation := snapshot.reservation.Reservation
			current.ProviderReservation = &reservation
		}
		result.CurrentLease = &current
		result.Scheduling.Outcome = "claimed"
		result.Scheduling.Reason = "A current fenced lease owns this item"
	}
	if result.CurrentLease == nil {
		for _, state := range snapshot.project.States {
			if state.Name == issue.State && !state.Dispatchable {
				result.Scheduling.Outcome = "skipped"
				result.Scheduling.Reason = issue.State + " is not dispatchable"
			}
		}
	}
	if attempt.Runtime == nil {
		result.Unavailable = append(result.Unavailable, "runtime_phase_heartbeat", "instruction_activity", "rest_accounting", "github_scope_timings", "attempt_landing_receipt", "host_issue_acceptance")
	} else {
		if attempt.Runtime.Activity == nil {
			result.Unavailable = append(result.Unavailable, "instruction_activity")
		}
		if attempt.Runtime.REST == nil {
			result.Unavailable = append(result.Unavailable, "rest_accounting")
		}
		if attempt.Runtime.GitHub == nil {
			result.Unavailable = append(result.Unavailable, "github_scope_timings")
		}
		if attempt.Runtime.Landing == nil {
			result.Unavailable = append(result.Unavailable, "attempt_landing_receipt")
		}
		if attempt.Runtime.Completion == nil {
			result.Unavailable = append(result.Unavailable, "host_issue_acceptance")
		}
	}
	if attempt.Finalization == nil {
		result.Unavailable = append(result.Unavailable, "host_finalization")
	}
	if attempt.TerminalFailure == nil {
		result.Unavailable = append(result.Unavailable, "terminal_failure")
	}
	if err := s.readRuntimeSourceRecoveryTenant(ctx, scope, item, &result); err != nil {
		return result, 0, false, err
	}
	return result, snapshot.boardSequence, true, nil
}

func selectedRuntimeAttempt(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, row corestore.WorkItem, attemptID string, localAttemptID int64) (leasestore.Attempt, error) {
	if localAttemptID > 0 {
		matches, err := r.LocalAttemptMatches(ctx, scope.Project, row.NativeID, localAttemptID)
		if errors.Is(err, leasestore.ErrLocalAttemptCoverageUnknown) {
			return leasestore.Attempt{}, hubapi.NativePublicInvalid("Historical local attempt coverage is incomplete; select native_attempt_id")
		}
		if err != nil {
			return leasestore.Attempt{}, err
		}
		switch len(matches) {
		case 0:
			return leasestore.Attempt{}, hubapi.NativeNotFound()
		case 1:
			attemptID = matches[0]
		default:
			return leasestore.Attempt{}, hubapi.NativePublicInvalid("Local attempt identity is ambiguous; select native_attempt_id")
		}
	}
	return r.Attempt(ctx, attemptID)
}
