package hubserver

import (
	"context"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
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
	attempt              tenantAttemptSnapshot
	currentLease         *leasestore.Lease
	attemptPolicy        policystore.Revision
	currentPolicy        policystore.Revision
	policyFound          bool
	leasePolicy          policystore.LeasePolicy
	currentPolicyBinding *policystore.LeasePolicy
	boardSequence        int64
	reservation          *claimstore.ProviderReservation
}

func (s *Service) readNativeRuntimeTenant(ctx context.Context, scope nativeScope, item, attemptID string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	if len(admission) != 0 {
		return tracker.NativeRuntimeEvidence{}, nativeRunEventStorageUnavailable()
	}
	if attemptID == "" {
		return tracker.NativeRuntimeEvidence{}, nativeRunEventStorageUnavailable()
	}
	if !validNativeID(attemptID, "attempt") {
		if local, err := strconv.ParseInt(attemptID, 10, 64); err == nil && local > 0 {
			return tracker.NativeRuntimeEvidence{}, nativeRunEventStorageUnavailable()
		}
		return tracker.NativeRuntimeEvidence{}, nativePublicInvalid("Invalid attempt identity")
	}
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok || s.runDetailStoreFor(ctx) == nil {
		return tracker.NativeRuntimeEvidence{}, nativeRunEventStorageUnavailable()
	}
	var snapshot nativeRuntimeTenantSnapshot
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{Kind: stringValue(row.ActorKind), PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		version, err := r.CollaborationVersion(ctx, item, row.Revision)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if attempt.OrganizationID != scope.organization || attempt.IssueID != row.ID {
			return nativeNotFound()
		}
		lease, err := r.Lease(ctx, attempt.LeaseID)
		if err != nil {
			return err
		}
		if lease.OrganizationID != scope.organization || lease.IssueID != row.ID || lease.FencingToken != attempt.FencingToken {
			return nativeNotFound()
		}
		event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
		if err != nil {
			return err
		}
		if event.OrganizationID != scope.organization || event.ProjectID != scope.project || event.WorkItemID != row.NativeID || event.AttemptID != attempt.ID {
			return corestore.ErrNotFound
		}
		leasePolicy, err := r.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
		if err != nil {
			return err
		}
		if leasePolicy.OrganizationID != scope.organization || leasePolicy.ProjectID != scope.project || leasePolicy.LeaseID != tracker.LeaseID(lease.LeaseID) || leasePolicy.PolicyID == "" {
			return policyMismatch("Lease policy is missing or outside the authorized project")
		}
		attemptPolicy, err := r.ProjectPolicyRevision(ctx, scope.project, leasePolicy.PolicyID)
		if err != nil {
			return err
		}
		currentPolicy, policyErr := r.ProjectPolicy(ctx, scope.project)
		policyFound := policyErr == nil
		if policyErr != nil && !errors.Is(policyErr, corestore.ErrNotFound) {
			return policyErr
		}
		board, err := r.BoardSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		snapshot.project, snapshot.item, snapshot.version = project, row, version
		snapshot.attempt = tenantAttemptSnapshot{attempt: attempt, lease: lease, event: event}
		snapshot.attemptPolicy, snapshot.currentPolicy, snapshot.policyFound = attemptPolicy, currentPolicy, policyFound
		snapshot.leasePolicy, snapshot.boardSequence = leasePolicy, board.Sequence
		activeLease, activeErr := r.ActiveLeaseForIssue(ctx, row.ID)
		if activeErr == nil {
			if activeLease.OrganizationID != scope.organization || activeLease.IssueID != row.ID {
				return corestore.ErrNotFound
			}
			snapshot.currentLease = &activeLease
			currentBinding, bindingErr := r.LeasePolicy(ctx, tracker.LeaseID(activeLease.LeaseID))
			if bindingErr != nil {
				return bindingErr
			}
			if currentBinding.OrganizationID != scope.organization || currentBinding.ProjectID != scope.project || currentBinding.LeaseID != tracker.LeaseID(activeLease.LeaseID) || currentBinding.PolicyID == "" {
				return policyMismatch("Current lease policy is missing or outside the authorized project")
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
		return tracker.NativeRuntimeEvidence{}, projectRecordError(err)
	}
	issue, err := s.hydrateWorkItemRecord(ctx, snapshot.item, snapshot.version)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, &issue); err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	issue.ChangeSequence = snapshot.boardSequence
	issue = issue.RuntimeReference()
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot.attempt, s.config.now().UTC())
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	if attempt.PolicyID == "" || attempt.PolicyID != snapshot.leasePolicy.PolicyID {
		return tracker.NativeRuntimeEvidence{}, policyMismatch("Attempt policy does not match its fenced lease")
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	boundPolicy, err := content.ProjectPolicy(ctx, snapshot.attemptPolicy)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	if err := boundPolicy.Validate(); err != nil || boundPolicy.ID != attempt.PolicyID {
		return tracker.NativeRuntimeEvidence{}, policyMismatch("Attempt policy reference is invalid")
	}
	if snapshot.policyFound {
		currentPolicy, err := content.ProjectPolicy(ctx, snapshot.currentPolicy)
		if err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
		if err := currentPolicy.Validate(); err != nil {
			return tracker.NativeRuntimeEvidence{}, err
		}
	}
	now := s.config.now().UTC()
	attempt.Current = snapshot.attempt.lease.ReleasedAt == nil && !now.Before(snapshot.attempt.lease.RenewedAt) && now.Before(snapshot.attempt.lease.ExpiresAt)
	if attempt.Runtime != nil {
		attempt.RuntimeFreshness = "available"
		heartbeat := attempt.Runtime.HeartbeatAt
		if heartbeat.IsZero() || now.Before(heartbeat) || snapshot.attempt.attempt.Status == "running" && (!attempt.Current || now.Sub(heartbeat) >= snapshot.attempt.lease.ExpiresAt.Sub(snapshot.attempt.lease.RenewedAt)) {
			attempt.RuntimeFreshness = "expired"
		}
	}
	attempt.Status = snapshot.attempt.attempt.Status
	if attempt.Status == "running" && !attempt.Current {
		attempt.Status = "interrupted"
	}
	result := tracker.NativeRuntimeEvidence{
		Issue: issue, Attempt: &attempt, ObservedAt: now, Selection: "selected",
		Unavailable: []string{"efficiency_receipt", "source_recovery_projection", "scheduler_history_projection", "change_gate_projection", "runner_capacity_projection", "native_admission_projection"},
		Capacity:    []tracker.NativeRuntimeCapacity{},
		Scheduling:  tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "unknown", At: now, WorkItemRevision: issue.Revision, Reason: "Runner capacity and admission projection are not available from this typed read"},
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
	return result, nil
}
