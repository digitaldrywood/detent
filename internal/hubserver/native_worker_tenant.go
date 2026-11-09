package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type preparedNativeLeaseCancellation struct {
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
}

func (s *Service) nativeLeaseReader(r tenantstore.Reader, scope nativeScope) *nativeLeaseCommandReader {
	_, compact := s.config.NativeRunEvents.(tenantNativeRunEventParticipant)
	return &nativeLeaseCommandReader{Reader: r, project: scope.project, compact: compact}
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

func (s *Service) nativeLeaseResponseDetails(ctx context.Context, r tenantstore.Reader, scope nativeScope, lease leasestore.Lease) (NativeLeaseResponseDetails, error) {
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
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Landing capacity must be non-negative"))
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Lease TTL is invalid"))
	}
	scope := nativeRequestScope(c)
	var lease leasestore.Lease
	var details NativeLeaseResponseDetails
	err = s.config.TenantRepository.WriteTenant(c.Request().Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		reader := s.nativeLeaseReader(w, scope)
		if _, err := reader.RunnerForLease(c.Request().Context(), c.Param("lease")); err != nil {
			return err
		}
		current, err := reader.Lease(c.Request().Context(), c.Param("lease"))
		if err != nil {
			return err
		}
		authority := NativeLeaseAuthority{Scope: scope, Lease: current, Request: request, Operation: "renew"}
		if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(c.Request().Context(), reader, &authority); err != nil {
			return err
		}
		lease, err = w.RenewWithLandingCapacity(c.Request().Context(), leasestore.Fence{LeaseID: c.Param("lease"), FencingToken: int64(request.FencingToken)}, ttl, int64(request.LandingCapacity))
		if err != nil {
			return err
		}
		authority.Lease = lease
		if err := s.config.NativeRunEvents.ApplyNativeLeaseMutation(c.Request().Context(), w, authority); err != nil {
			return err
		}
		reader.lease = &lease
		details, err = s.nativeLeaseResponseDetails(c.Request().Context(), reader, scope, lease)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.writeNativeLeaseResponse(c, lease, details)
}

func (s *Service) releaseNativeLeaseTenant(c echo.Context) error {
	if s.config.NativeRunEvents == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.CancelAttempt && request.Reason != "cancelled" || !slices.Contains([]string{"completed", "cancelled", "failed", "work_item_hydration_failed", "work_item_identity_missing", "released"}, request.Reason) {
		return s.nativeAPIError(c, nativePublicInvalid("Release reason is invalid"))
	}
	scope := nativeRequestScope(c)
	leaseID := c.Param("lease")
	prepared, err := s.prepareNativeLeaseCancellationTenant(c.Request().Context(), scope, leaseID, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	command := eventstore.RunCommand{Project: scope.project, LeaseID: leaseID, Operation: "release"}
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
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		lease, err := reader.Lease(ctx, leaseID)
		if err != nil {
			return err
		}
		item, err := reader.WorkItemByID(ctx, scope.project, lease.IssueID)
		if err != nil {
			return err
		}
		lockedItem, err := w.LockWorkItem(ctx, scope.project, item.NativeID)
		if err != nil {
			return err
		}
		reader.item = &lockedItem
		authority := NativeLeaseAuthority{Scope: scope, Lease: lease, Request: request, Operation: "release"}
		if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(ctx, reader, &authority); err != nil {
			return err
		}
		if prepared.attempt.ID != "" {
			attempt, err := w.LatestAttempt(ctx, lease.IssueID)
			if err != nil {
				return err
			}
			if attempt.ID != prepared.attempt.ID || attempt.LeaseID != prepared.attempt.LeaseID || attempt.FencingToken != prepared.attempt.FencingToken || attempt.Sequence != prepared.attempt.Sequence || attempt.Status != "running" {
				return leasestore.ErrAttemptConflict
			}
			workItem, err := w.WorkItem(ctx, scope.project, prepared.workItem.NativeID)
			if err != nil {
				return err
			}
			if workItem.ID != lease.IssueID {
				return corestore.ErrNotFound
			}
			attemptEvent := leasestore.AttemptEvent{ID: attempt.ID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: attempt.RunID,
				Sequence: attempt.Sequence + 1, Status: "cancelled", DispatchGeneration: attempt.DispatchGeneration, PayloadObjectKey: prepared.payloadReference.Key, PayloadReference: prepared.payloadReference}
			runAuthority := NativeRunEventAuthority{Scope: scope, OrganizationID: scope.organization, ProjectID: scope.project, WorkItem: workItem, Lease: lease, Event: prepared.event}
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
			if err := s.config.NativeRunEvents.ApplyNativeRunEvent(ctx, w, runAuthority); err != nil {
				return err
			}
		}
		if _, err := w.Release(ctx, leasestore.Fence{LeaseID: leaseID, FencingToken: int64(request.FencingToken)}, request.Reason); err != nil {
			return err
		}
		if err := s.config.NativeRunEvents.ApplyNativeLeaseMutation(ctx, w, authority); err != nil {
			return err
		}
		if s.conversations != nil {
			return s.conversations.leaseReleasedTenant(ctx, w, leaseID, authority.Lease.FencingToken, request.Reason)
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.signalNativeDispatch(scope)
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) prepareNativeLeaseCancellationTenant(ctx context.Context, scope nativeScope, leaseID string, request tracker.NativeLeaseMutation) (*preparedNativeLeaseCancellation, error) {
	return s.prepareLeaseCancellationTenant(ctx, scope, leaseID, request, false)
}

func (s *Service) prepareLeaseCancellationTenant(ctx context.Context, scope nativeScope, leaseID string, request tracker.NativeLeaseMutation, human bool) (*preparedNativeLeaseCancellation, error) {
	if s.runDetailStore() == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	cancellation := &preparedNativeLeaseCancellation{}
	var lease leasestore.Lease
	var attempt leasestore.Attempt
	var event eventstore.Event
	var workItem corestore.WorkItem
	err := s.readNativeRunCommand(ctx, scope, eventstore.RunCommand{Project: scope.project, LeaseID: leaseID}, func(ctx context.Context, r tenantstore.Reader) error {
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
		event, err = r.LatestRunEvent(ctx, scope.project, attempt.ID)
		if err != nil {
			return err
		}
		if event.WorkItemID == "" {
			return corestore.ErrNotFound
		}
		workItem, err = r.WorkItem(ctx, scope.project, event.WorkItemID)
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
	if event.AttemptID != attempt.ID || event.RunID != attempt.RunID {
		return nil, artifact.ErrIntegrity
	}
	stored, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, event)
	if err != nil {
		return nil, err
	}
	if stored.Data.Run == nil || stored.Data.Run.Sequence != attempt.Sequence {
		return nil, artifact.ErrIntegrity
	}
	data := *stored.Data.Run
	data.Sequence = attempt.Sequence + 1
	data.Outcome = "cancelled"
	data.Handoff, data.ArtifactIDs = nil, nil
	data.MachineID, data.SessionID = tracker.MachineID(lease.MachineID), lease.SessionID
	if !human {
		data.RunnerID = scope.credential.Runner.RunnerID
	}
	now := s.config.now().UTC()
	failure := nativeLeaseReleaseFailure(request.Reason, now)
	data.TerminalFailure = &failure
	prepared := tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, OccurredAt: now, Data: data}
	if err := validateNativeRunEvent(prepared); err != nil {
		return nil, err
	}
	data = prepared.Data
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	ref, err := s.runDetailStore().StoreDetail(ctx, string(scope.organization), encoded)
	if err != nil {
		return nil, err
	}
	if err := validateNativeDetailReference(ctx, s.runDetailStore(), scope.organization, ref, encoded); err != nil {
		return nil, err
	}
	eventValue := tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: stored.AggregateID, Type: "run.finished", SchemaVersion: 1, RecordedAt: now,
		Actor: tracker.Actor{Kind: "integration", PrincipalID: "lease_release"}, Data: tracker.CollaborationData{Run: &data}}
	raw, err := json.Marshal(eventValue)
	if err != nil {
		return nil, err
	}
	history, err := eventstore.NewContent(s.runDetailStore()).PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return nil, err
	}
	*cancellation = preparedNativeLeaseCancellation{attempt: attempt, workItem: workItem, payloadReference: ref, event: prepared, history: history}
	return cancellation, nil
}

func (s *Service) respondNativeLeaseTenant(c echo.Context, scope nativeScope, leaseID string) error {
	if s.config.NativeRunEvents == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
	var lease leasestore.Lease
	var details NativeLeaseResponseDetails
	err := s.config.TenantRepository.ReadTenant(c.Request().Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {

		reader := s.nativeLeaseReader(r, scope)
		var err error
		lease, err = reader.Lease(c.Request().Context(), leaseID)
		if err != nil {
			return err
		}
		details, err = s.nativeLeaseResponseDetails(c.Request().Context(), reader, scope, lease)
		return err
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
	return c.JSON(http.StatusOK, tracker.NativeLease{RunDetailBatching: s.runDetailStore() != nil, IsolationPolicy: details.IsolationPolicy,
		ProviderReservation: details.ProviderReservation, ServerTime: s.config.now().UTC(), PolicyID: details.PolicyID,
		ID: tracker.LeaseID(lease.LeaseID), WorkItemID: details.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID,
		FencingToken: tracker.FencingToken(lease.FencingToken), AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt})
}
