package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimapi"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errLatestRunEventMismatch = fmt.Errorf("latest run event does not continue the running attempt: %w", artifact.ErrIntegrity)

type preparedNativeLeaseCancellation struct {
	expired          bool
	attempt          leasestore.Attempt
	workItem         corestore.WorkItem
	payloadReference attachment.ArchiveReference
	event            tracker.NativeRunEvent
	history          eventstore.Event
}

type nativeLeaseCommandReader struct {
	tenantstore.Reader
	lease            *leasestore.Lease
	item             *corestore.WorkItem
	runner           *runnerstore.Identity
	runnerLease      string
	reservation      claimstore.ProviderReservation
	reservationLease tracker.LeaseID
	reservationErr   error
	project          tracker.ProjectID
	compact          bool
	policiesLoaded   bool
	policiesErr      error
	policyLease      tracker.LeaseID
	pinnedPolicy     policystore.LeasePolicy
	monthlyPolicies  []billingstore.MonthlyPolicy
}

func (s *Service) nativeLeaseReader(r tenantstore.Reader, scope hubscope.NativeScope) *nativeLeaseCommandReader {
	_, compact := s.config.NativeRunEvents.(tenantNativeRunEventParticipant)
	return &nativeLeaseCommandReader{Reader: r, project: scope.Project, compact: compact}
}

func (r *nativeLeaseCommandReader) Lease(ctx context.Context, id string) (leasestore.Lease, error) {
	if r.lease != nil && r.lease.LeaseID == id {
		return *r.lease, nil
	}
	if facts, ok := r.Reader.(interface {
		LeaseMutationFacts(context.Context, tracker.ProjectID, string) (leasestore.Lease, corestore.WorkItem, error)
	}); ok && r.compact {
		lease, item, err := facts.LeaseMutationFacts(ctx, r.project, id)
		if err == nil {
			r.lease, r.item = &lease, &item
		}
		return lease, err
	}
	row, err := r.Reader.Lease(ctx, id)
	if err == nil {
		r.lease = &row
	}
	return row, err
}

func (r *nativeLeaseCommandReader) WorkItemByID(ctx context.Context, project tracker.ProjectID, id int64) (corestore.WorkItem, error) {
	if r.item != nil && r.item.ProjectID == project && r.item.ID == id {
		return *r.item, nil
	}
	row, err := r.Reader.WorkItemByID(ctx, project, id)
	if err == nil {
		r.item = &row
	}
	return row, err
}

func (r *nativeLeaseCommandReader) RunnerForLease(ctx context.Context, id string) (runnerstore.Identity, error) {
	if r.runner != nil && r.runnerLease == id {
		return *r.runner, nil
	}
	var row runnerstore.Identity
	var err error
	if compact, ok := r.Reader.(interface {
		LeaseMutationIdentity(context.Context, string) (runnerstore.Identity, error)
	}); ok && r.compact {
		row, err = compact.LeaseMutationIdentity(ctx, id)
	} else {
		row, err = r.Reader.RunnerForLease(ctx, id)
	}
	if err == nil {
		r.runner, r.runnerLease = &row, id
	}
	return row, err
}

func (r *nativeLeaseCommandReader) loadPolicies(ctx context.Context) bool {
	if !r.compact || r.lease == nil {
		return false
	}
	owner, ok := r.Reader.(interface {
		LeaseMutationPolicies(context.Context, tracker.ProjectID, tracker.LeaseID) (policystore.LeasePolicy, []billingstore.MonthlyPolicy, error)
	})
	if !ok {
		return false
	}
	if !r.policiesLoaded {
		r.policiesLoaded = true
		r.policyLease = tracker.LeaseID(r.lease.LeaseID)
		r.pinnedPolicy, r.monthlyPolicies, r.policiesErr = owner.LeaseMutationPolicies(ctx, r.project, r.policyLease)
	}
	return true
}

func (r *nativeLeaseCommandReader) BillingMonthlyPolicy(ctx context.Context, project tracker.ProjectID) (billingstore.MonthlyPolicy, error) {
	if project != "" && project != r.project || !r.loadPolicies(ctx) {
		return r.Reader.BillingMonthlyPolicy(ctx, project)
	}
	if r.policiesErr != nil {
		return billingstore.MonthlyPolicy{}, r.policiesErr
	}
	for _, row := range r.monthlyPolicies {
		if row.ProjectID == project {
			return row, nil
		}
	}
	return billingstore.MonthlyPolicy{}, corestore.ErrNotFound
}

func (r *nativeLeaseCommandReader) LeasePolicy(ctx context.Context, lease tracker.LeaseID) (policystore.LeasePolicy, error) {
	if r.lease == nil || lease != tracker.LeaseID(r.lease.LeaseID) || !r.loadPolicies(ctx) || lease != r.policyLease {
		return r.Reader.LeasePolicy(ctx, lease)
	}
	return r.pinnedPolicy, r.policiesErr
}

func (r *nativeLeaseCommandReader) ProviderReservation(ctx context.Context, id tracker.LeaseID) (claimstore.ProviderReservation, error) {
	if r.reservationLease == id && id != "" {
		return r.reservation, r.reservationErr
	}
	var row claimstore.ProviderReservation
	var err error
	if compact, ok := r.Reader.(interface {
		LeaseMutationReservation(context.Context, tracker.LeaseID) (claimstore.ProviderReservation, error)
	}); ok && r.compact {
		row, err = compact.LeaseMutationReservation(ctx, id)
	} else {
		row, err = r.Reader.ProviderReservation(ctx, id)
	}
	if err == nil || errors.Is(err, corestore.ErrNotFound) {
		r.reservationLease, r.reservation, r.reservationErr = id, row, err
	}
	return row, err
}

func (s *Service) nativeLeaseResponseDetails(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, lease leasestore.Lease) (NativeLeaseResponseDetails, error) {
	details, err := s.config.NativeRunEvents.NativeLeaseResponse(ctx, r, scope, lease)
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	details.ProviderReservation = nil
	reservation, found, err := readTenantProviderReservation(ctx, r, tracker.LeaseID(lease.LeaseID))
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	if found {
		details.ProviderReservation = &reservation.Reservation
	}
	return details, nil
}

func (s *Service) renewNativeLeaseTenant(c echo.Context) error {
	if s.config.NativeRunEvents == nil {
		return s.nativeAPIError(c, hubapi.NativeRunEventStorageUnavailable())
	}
	var request tracker.NativeLeaseMutation
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Landing capacity must be non-negative"))
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Lease TTL is invalid"))
	}
	scope := hubscope.NativeRequestScope(c)
	providerPlan, err := s.prepareNativeAuditProvider(c.Request().Context(), scope, tracker.LeaseID(c.Param("lease")), request.ProviderPhase)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	landing, err := s.prepareNativeLandingRenewal(c.Request().Context(), scope, c.Param("lease"), request.LandingCapacity)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var lease leasestore.Lease
	var details NativeLeaseResponseDetails
	err = s.renewNativeLeaseWrite(c.Request().Context(), scope, tracker.LeaseID(c.Param("lease")), func(legacySelectorChecked bool) error {
		return s.config.TenantRepository.WriteTenant(c.Request().Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			reader := s.nativeLeaseReader(w, scope)
			current, err := reader.Lease(c.Request().Context(), c.Param("lease"))
			if err != nil {
				return err
			}
			if landing == nil && current.LandingCapacity > 0 {
				landing = &nativeLandingRenewal{lease: current}
			}
			if landing != nil {
				if current.RunnerID != nil && *current.RunnerID != scope.Credential.Runner.RunnerID {
					return tracker.ErrMachineNotFound
				}
				if err := lockRunnerCredential(c.Request().Context(), w, scope); err != nil {
					return err
				}
				if err := w.LockWorkItemProject(c.Request().Context(), scope.Project); err != nil {
					return err
				}
				reader = s.nativeLeaseReader(w, scope)
			}
			if _, err := reader.RunnerForLease(c.Request().Context(), c.Param("lease")); err != nil {
				return err
			}
			current, err = reader.Lease(c.Request().Context(), c.Param("lease"))
			if err != nil {
				return err
			}
			authority := NativeLeaseAuthority{Scope: scope, Lease: current, Request: request, Operation: "renew", LegacySelectorChecked: legacySelectorChecked}
			if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(c.Request().Context(), reader, &authority); err != nil {
				return err
			}
			landingCapacity := int64(request.LandingCapacity)
			if current.LandingCapacity > 0 && (landingCapacity == 0 || landingCapacity > current.LandingCapacity) {
				landingCapacity = current.LandingCapacity
			}
			landingCapacity, err = s.reserveNativeLandingRenewal(c.Request().Context(), w, scope, landing, current, landingCapacity)
			if err != nil {
				return err
			}
			lease, err = w.RenewWithLandingCapacity(c.Request().Context(), leasestore.Fence{LeaseID: c.Param("lease"), FencingToken: int64(request.FencingToken)}, ttl, landingCapacity)
			if err != nil {
				return err
			}
			if request.ProviderPhase != nil {
				if err := s.setLeaseProviderPhase(c.Request().Context(), w, scope, lease, *request.ProviderPhase, providerPlan); err != nil {
					return err
				}
				reader = s.nativeLeaseReader(w, scope)
			}
			authority.Lease = lease
			if err := s.config.NativeRunEvents.ApplyNativeLeaseMutation(c.Request().Context(), w, authority); err != nil {
				return err
			}
			reader.lease = &lease
			details, err = s.nativeLeaseResponseDetails(c.Request().Context(), reader, scope, lease)
			return err
		})
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.writeNativeLeaseResponse(c, lease, details)
}

func (s *Service) setLeaseProviderPhase(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, lease leasestore.Lease, phase tracker.NativeProviderPhaseMutation, providerPlan *nativeAuditProviderPlan) error {
	id := tracker.LeaseID(lease.LeaseID)
	attempt, err := w.LatestAttempt(ctx, lease.IssueID)
	if err != nil {
		return err
	}
	if attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Status != "running" {
		return hubapi.NativeStaleExecution("Provider phases require the existing live item attempt")
	}
	if attempt.Sequence != phase.Sequence {
		return claimapi.ProviderWait("provider_phase_changed", "Provider phase belongs to an earlier attempt event")
	}
	pinned, err := w.LeasePolicy(ctx, id)
	if err != nil {
		return err
	}
	policies, err := w.PolicyCompatibility(ctx, scope.Project, []string{pinned.PolicyID})
	if err != nil {
		return err
	}
	facts := policies[pinned.PolicyID]
	requirement := phase.Requirement
	release := requirement == (providercapacity.Requirement{})
	if providerPlan != nil {
		if !facts.Gates.SecurityAudit {
			return hubapi.PolicyMismatch("The pinned policy does not enable security audit")
		}
		if err := providerPlan.authorize(ctx, w, scope, attempt, pinned, phase); err != nil {
			return err
		}
	} else if !facts.Gates.PlanEnabled || phase.AuditPredecessor != 0 {
		return hubapi.PolicyMismatch("The pinned policy does not authorize the provider phase")
	}
	if !release && (requirement.Validate() != nil || !slices.Contains([]string{"plan", "code", "rework", "merge", "security_audit"}, requirement.Role)) {
		return hubapi.NativePublicInvalid("Provider phase requires a valid execution identity")
	}
	if requirement.Role == "merge" && (providerPlan == nil || providerPlan.event == nil) {
		return hubapi.PolicyMismatch("Merge capacity must come from the landing attempt")
	}
	current, found, err := readTenantProviderReservation(ctx, w, id)
	if err != nil {
		return err
	}
	active := current.Reservation.Requirement
	if !found {
		active = providercapacity.Requirement{}
	}
	if active.Role == "security_audit" && providerPlan == nil {
		return hubapi.PolicyMismatch("The audit phase requires its authorized successor")
	}
	if active == requirement {
		return nil
	}
	if active != phase.Expected {
		return claimapi.ProviderWait("provider_phase_changed", "Current provider permit differs from the phase being retired")
	}
	if err := w.DropProviderReservation(ctx, id); err != nil || release {
		return err
	}
	reservation, err := claimapi.SelectTenantProviderRequirement(ctx, w, scope.Credential.Runner.RunnerID, requirement, s.config.Clock().UTC())
	if err != nil {
		return err
	}
	return w.CreateProviderReservation(ctx, claimstore.ProviderReservation{OrganizationID: scope.Organization, LeaseID: id, Reservation: reservation})
}

func (s *Service) renewNativeLeaseWrite(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID, write func(legacySelectorChecked bool) error) error {
	err := write(false)
	if !errors.Is(err, errLeaseSelectorUnbound) {
		return err
	}
	if err := s.checkLegacyLeaseSelector(ctx, scope, leaseID); err != nil {
		return err
	}
	return write(true)
}

func (s *Service) releaseNativeLeaseTenant(c echo.Context) error {
	if s.config.NativeRunEvents == nil {
		return s.nativeAPIError(c, hubapi.NativeRunEventStorageUnavailable())
	}
	var request tracker.NativeLeaseMutation
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.CancelAttempt && request.Reason != "cancelled" || !slices.Contains([]string{"completed", "cancelled", "failed", "work_item_hydration_failed", "work_item_identity_missing", "released"}, request.Reason) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Release reason is invalid"))
	}
	scope := hubscope.NativeRequestScope(c)
	leaseID := c.Param("lease")
	prepared, err := s.prepareNativeLeaseCancellationTenant(c.Request().Context(), scope, leaseID, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	command := eventstore.RunCommand{Project: scope.Project, LeaseID: leaseID, Operation: "release"}
	if prepared.attempt.ID != "" {
		command.WorkItem, command.AttemptID = prepared.workItem.NativeID, prepared.attempt.ID
		command.Sequences = []int64{prepared.attempt.Sequence + 1}
		command.EventIDs = []string{prepared.history.ID}
	}
	err = s.writeNativeRunCommand(c.Request().Context(), scope, command, func(ctx context.Context, w tenantstore.Writer) error {
		reader := s.nativeLeaseReader(w, scope)
		if _, err := reader.RunnerForLease(ctx, leaseID); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		lease, err := reader.Lease(ctx, leaseID)
		if err != nil {
			return err
		}
		item, err := reader.WorkItemByID(ctx, scope.Project, lease.IssueID)
		if err != nil {
			return err
		}
		lockedItem, err := w.LockWorkItem(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		reader.item = &lockedItem
		authority := NativeLeaseAuthority{Scope: scope, Lease: lease, Request: request, Operation: "release"}
		if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(ctx, reader, &authority); err != nil {
			return err
		}
		if err := s.recordPreparedLeaseCancellation(ctx, w, scope, lease, prepared); err != nil {
			return err
		}
		if _, err := w.Release(ctx, leasestore.Fence{LeaseID: leaseID, FencingToken: int64(request.FencingToken)}, request.Reason); err != nil {
			return err
		}
		if err := s.config.NativeRunEvents.ApplyNativeLeaseMutation(ctx, w, authority); err != nil {
			return err
		}
		if s.conversations != nil {
			return s.conversations.LeaseReleasedTenant(ctx, w, leaseID, authority.Lease.FencingToken, request.Reason)
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.signalNativeDispatch(scope)
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) recordPreparedLeaseCancellation(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, lease leasestore.Lease, prepared *preparedNativeLeaseCancellation) error {
	if prepared == nil || prepared.attempt.ID == "" {
		return nil
	}
	attempt, err := w.LatestAttempt(ctx, lease.IssueID)
	if err != nil {
		return err
	}
	if attempt.ID != prepared.attempt.ID || attempt.LeaseID != prepared.attempt.LeaseID || attempt.FencingToken != prepared.attempt.FencingToken || attempt.Sequence != prepared.attempt.Sequence || attempt.Status != "running" {
		return leasestore.ErrAttemptConflict
	}
	workItem, err := w.WorkItem(ctx, scope.Project, prepared.workItem.NativeID)
	if err != nil {
		return err
	}
	if workItem.ID != lease.IssueID {
		return corestore.ErrNotFound
	}
	attemptEvent := leasestore.AttemptEvent{ID: attempt.ID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: attempt.RunID,
		Sequence: attempt.Sequence + 1, Status: "cancelled", DispatchGeneration: attempt.DispatchGeneration, PayloadObjectKey: prepared.payloadReference.Key, PayloadReference: prepared.payloadReference, WorkerCleanupRequired: new(nativeRunCleanupRequired(prepared.event.Data)), Failure: attemptFailureSummary(prepared.event.Data, "cancelled"), Identity: leasestore.NewAttemptIdentity(prepared.event.Data)}
	runAuthority := NativeRunEventAuthority{Scope: scope, OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItem: workItem, Lease: lease, Event: prepared.event}
	if err := s.config.NativeRunEvents.ValidateNativeRunEvent(ctx, w, &runAuthority); err != nil {
		return err
	}
	attemptEvent.DispatchGeneration = runAuthority.DispatchGeneration
	stored, duplicate, err := w.RecordAttemptEvent(ctx, attemptEvent)
	if err != nil {
		return err
	}
	if duplicate || stored.ID != attempt.ID || stored.Sequence != attemptEvent.Sequence || stored.Status != "cancelled" {
		return leasestore.ErrAttemptConflict
	}
	if _, err := w.AppendEvent(ctx, prepared.history); err != nil {
		return err
	}
	return s.config.NativeRunEvents.ApplyNativeRunEvent(ctx, w, runAuthority)
}

func (s *Service) prepareNativeLeaseCancellationTenant(ctx context.Context, scope hubscope.NativeScope, leaseID string, request tracker.NativeLeaseMutation) (*preparedNativeLeaseCancellation, error) {
	return s.prepareLeaseCancellationTenant(ctx, scope, leaseID, request, false)
}

func (s *Service) prepareLeaseCancellationTenant(ctx context.Context, scope hubscope.NativeScope, leaseID string, request tracker.NativeLeaseMutation, human bool) (*preparedNativeLeaseCancellation, error) {
	if s.runDetailStore() == nil {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	cancellation := &preparedNativeLeaseCancellation{}
	var lease leasestore.Lease
	var attempt leasestore.Attempt
	var event eventstore.Event
	var workItem corestore.WorkItem
	err := s.readNativeRunCommand(ctx, scope, eventstore.RunCommand{Project: scope.Project, LeaseID: leaseID}, func(ctx context.Context, r tenantstore.Reader) error {
		r = s.nativeLeaseReader(r, scope)
		var err error
		lease, err = r.Lease(ctx, leaseID)
		if err != nil {
			return err
		}
		authority := NativeLeaseAuthority{Scope: scope, Lease: lease, Request: request, Operation: "release"}
		var validationErr error
		if human {
			validationErr = (tenantNativeRunEventParticipant{service: s}).validateNativeLeaseMutation(ctx, r, &authority, true)
		} else {
			validationErr = s.config.NativeRunEvents.ValidateNativeLeaseMutation(ctx, r, &authority)
		}
		if validationErr != nil {
			return validationErr
		}
		attempt, err = r.LatestAttempt(ctx, lease.IssueID)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, corestore.ErrNotFound) || errors.Is(err, leasestore.ErrAttemptConflict) {
			return nil
		}
		if err != nil {
			return err
		}
		if attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Status != "running" {
			attempt = leasestore.Attempt{}
			return nil
		}
		event, err = r.LatestRunEvent(ctx, scope.Project, attempt.ID)
		if err != nil {
			return err
		}
		if event.WorkItemID == "" {
			return corestore.ErrNotFound
		}
		workItem, err = r.WorkItem(ctx, scope.Project, event.WorkItemID)
		if err != nil || workItem.ID != lease.IssueID {
			if err != nil {
				return err
			}
			return corestore.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if attempt.ID == "" {
		return cancellation, nil
	}
	runnerID := ""
	if !human {
		runnerID = scope.Credential.Runner.RunnerID
	}
	prepared, err := s.prepareLeaseCancellationContent(ctx, scope.Organization, scope.Project, lease, attempt, workItem, event, request.Reason, runnerID)
	if errors.Is(err, errLatestRunEventMismatch) {
		return nil, nativeExecutionConflict("Latest run event does not belong to the lease's running attempt")
	}
	return prepared, err
}

func (s *Service) prepareExpiredNativeLeaseCancellation(ctx context.Context, scope hubscope.NativeScope, issueID int64, includeReleased bool) (*preparedNativeLeaseCancellation, error) {
	var lease leasestore.Lease
	var attempt leasestore.Attempt
	var workItem corestore.WorkItem
	var event eventstore.Event
	var policyID string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.LeaseLossCandidate(ctx, issueID, includeReleased)
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		lease, err = r.Lease(ctx, current.LeaseID)
		if err != nil {
			return err
		}
		if lease.IssueID != issueID || lease.FencingToken != current.FencingToken {
			return artifact.ErrIntegrity
		}
		workItem, err = r.WorkItemByID(ctx, scope.Project, issueID)
		if err != nil {
			return err
		}
		event, err = r.LatestRunEvent(ctx, scope.Project, current.ID)
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			event, err = eventstore.Event{}, nil
			bound, policyErr := r.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
			if policyErr != nil && !errors.Is(policyErr, corestore.ErrNotFound) && !errors.Is(policyErr, sql.ErrNoRows) {
				return policyErr
			}
			policyID = bound.PolicyID
		}
		if err != nil {
			return err
		}
		if event.AttemptID != "" && event.WorkItemID != workItem.NativeID {
			return artifact.ErrIntegrity
		}
		attempt = current
		return nil
	})
	if err != nil || attempt.ID == "" {
		return nil, err
	}
	reason := lease.ReleaseReason
	if reason == "" && lease.ReleasedAt == nil {
		reason = "expired"
	}
	var prepared *preparedNativeLeaseCancellation
	if event.AttemptID == "" {
		prepared, err = s.prepareAttemptCancellationContent(ctx, scope.Organization, scope.Project, lease, attempt, workItem, policyID, reason)
	} else {
		prepared, err = s.prepareLeaseCancellationContent(ctx, scope.Organization, scope.Project, lease, attempt, workItem, event, reason, "")
	}
	if err != nil {
		return nil, err
	}
	prepared.expired = true
	return prepared, nil
}

func (p *preparedNativeLeaseCancellation) expiresLease(lease leasestore.Lease) bool {
	return p != nil && p.expired && p.attempt.LeaseID == lease.LeaseID && p.attempt.FencingToken == lease.FencingToken
}

func (p *preparedNativeLeaseCancellation) attemptCancellation() *leasestore.AttemptCancellation {
	if p == nil || !p.expired {
		return nil
	}
	attempt := p.attempt
	return &leasestore.AttemptCancellation{
		Attempt: leasestore.AttemptEvent{ID: attempt.ID, RunID: attempt.RunID, Fence: leasestore.Fence{LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken},
			Sequence: attempt.Sequence + 1, Status: "cancelled", DispatchGeneration: attempt.DispatchGeneration,
			PayloadObjectKey: p.payloadReference.Key, PayloadReference: p.payloadReference, WorkerCleanupRequired: new(nativeRunCleanupRequired(p.event.Data)), Failure: attemptFailureSummary(p.event.Data, "cancelled"), Identity: leasestore.NewAttemptIdentity(p.event.Data)},
		History: p.history,
	}
}

func (s *Service) prepareLeaseCancellationContent(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, lease leasestore.Lease, attempt leasestore.Attempt, workItem corestore.WorkItem, event eventstore.Event, reason, runnerID string) (*preparedNativeLeaseCancellation, error) {
	if s.runDetailStore() == nil {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	store := hubrecords.RunEventDetailStore{DetailStore: s.runDetailStore()}
	if event.AttemptID != attempt.ID || event.RunID != attempt.RunID {
		return nil, errLatestRunEventMismatch
	}
	stored, err := eventstore.NewContent(store).Event(ctx, event)
	if err != nil {
		return nil, err
	}
	if stored.Data.Run == nil || stored.Data.Run.Sequence != attempt.Sequence {
		return nil, errLatestRunEventMismatch
	}
	data := *stored.Data.Run
	if runnerID != "" {
		data.RunnerID = runnerID
	}
	return s.prepareCancelledRunContent(ctx, organization, project, lease, attempt, workItem, stored.AggregateID, data, reason, true)
}

func (s *Service) prepareAttemptCancellationContent(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, lease leasestore.Lease, attempt leasestore.Attempt, workItem corestore.WorkItem, policyID, reason string) (*preparedNativeLeaseCancellation, error) {
	if s.runDetailStore() == nil {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	if attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.IssueID != workItem.ID {
		return nil, artifact.ErrIntegrity
	}
	data := tracker.NativeRunData{LeaseID: tracker.LeaseID(attempt.LeaseID), FencingToken: tracker.FencingToken(attempt.FencingToken), RunID: attempt.RunID, AttemptID: attempt.ID, PolicyID: policyID}
	if lease.RunnerID != nil {
		data.RunnerID = *lease.RunnerID
	}
	return s.prepareCancelledRunContent(ctx, organization, project, lease, attempt, workItem, workItem.NativeID, data, reason, false)
}

func (s *Service) prepareCancelledRunContent(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, lease leasestore.Lease, attempt leasestore.Attempt, workItem corestore.WorkItem, aggregate tracker.NativeWorkItemID, data tracker.NativeRunData, reason string, validate bool) (*preparedNativeLeaseCancellation, error) {
	store := hubrecords.RunEventDetailStore{DetailStore: s.runDetailStore()}
	data.Sequence = attempt.Sequence + 1
	data.Outcome = "cancelled"
	data.Handoff, data.ArtifactIDs = nil, nil
	data.MachineID, data.SessionID = tracker.MachineID(lease.MachineID), lease.SessionID
	now := s.config.Clock().UTC()
	failure := nativeLeaseReleaseFailure(reason, now)
	data.TerminalFailure = &failure
	prepared := tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, OccurredAt: now, Data: data}
	if validate {
		if err := validateNativeRunEvent(prepared); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	ref, err := store.StoreDetail(ctx, string(organization), encoded)
	if err != nil {
		return nil, err
	}
	if err := validateNativeDetailReference(ctx, store, organization, ref, encoded); err != nil {
		return nil, err
	}
	eventValue := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: organization, ProjectID: project, AggregateType: "issue", AggregateID: aggregate, Type: "run.finished", SchemaVersion: 1, RecordedAt: now,
		Actor: tracker.Actor{Kind: "integration", PrincipalID: "lease_release"}, Data: tracker.CollaborationData{Run: &data}}
	raw, err := json.Marshal(eventValue)
	if err != nil {
		return nil, err
	}
	history, err := eventstore.NewContent(store).PrepareEvent(ctx, organization, raw)
	if err != nil {
		return nil, err
	}
	return &preparedNativeLeaseCancellation{attempt: attempt, workItem: workItem, payloadReference: ref, event: prepared, history: history}, nil
}

func (s *Service) respondNativeLeaseTenant(c echo.Context, scope hubscope.NativeScope, leaseID string) error {
	if s.config.NativeRunEvents == nil {
		return s.nativeAPIError(c, hubapi.NativeRunEventStorageUnavailable())
	}
	var lease leasestore.Lease
	var details NativeLeaseResponseDetails
	ctx := c.Request().Context()
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader := s.nativeLeaseReader(r, scope)
		var err error
		lease, err = reader.Lease(ctx, leaseID)
		if err != nil {
			return err
		}
		details, err = s.nativeLeaseResponseDetails(ctx, reader, scope, lease)
		if err != nil {
			return err
		}
		if details.WorkItemID == "" {
			return corestore.ErrNotFound
		}
		counts, err := r.ClaimOpenBlockerCounts(ctx, tenantstore.ClaimBlockerQuery{Project: scope.Project, WorkItems: []tracker.NativeWorkItemID{details.WorkItemID}, CredentialID: scope.Credential.ID, Now: s.config.Clock(), SampleLimit: hubrecords.NativeClaimBlockerLimit + 1})
		if err != nil {
			return err
		}
		summary := hubrecords.SummarizeNativeClaimBlockers(counts[details.WorkItemID])
		details.DependencyFacts = &summary
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.writeNativeLeaseResponse(c, lease, details)
}

func (s *Service) writeNativeLeaseResponse(c echo.Context, lease leasestore.Lease, details NativeLeaseResponseDetails) error {
	if details.WorkItemID == "" {
		return s.nativeAPIError(c, corestore.ErrNotFound)
	}
	response := tracker.NativeLease{RunDetailBatching: s.runDetailStore() != nil, IsolationPolicy: details.IsolationPolicy,
		ProviderReservation: details.ProviderReservation, ServerTime: s.config.Clock().UTC(), PolicyID: details.PolicyID,
		ID: tracker.LeaseID(lease.LeaseID), WorkItemID: details.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID,
		FencingToken: tracker.FencingToken(lease.FencingToken), AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt}
	return c.JSON(http.StatusOK, struct {
		tracker.NativeLease
		*hubrecords.NativeClaimBlockerSummary
	}{response, details.DependencyFacts})
}
