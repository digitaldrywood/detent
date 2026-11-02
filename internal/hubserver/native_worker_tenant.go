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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type preparedNativeLeaseCancellation struct {
	attempt          leasestore.Attempt
	workItem         corestore.WorkItem
	payloadObjectKey string
	event            tracker.NativeRunEvent
	history          eventstore.Event
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
	err = s.config.TenantRepository.WriteTenant(c.Request().Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.Lease(c.Request().Context(), c.Param("lease"))
		if err != nil {
			return err
		}
		authority := NativeLeaseAuthority{Scope: scope, Lease: current, Request: request, Operation: "renew"}
		if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(c.Request().Context(), w, &authority); err != nil {
			return err
		}
		lease, err = w.RenewWithLandingCapacity(c.Request().Context(), leasestore.Fence{LeaseID: c.Param("lease"), FencingToken: int64(request.FencingToken)}, ttl, int64(request.LandingCapacity))
		if err != nil {
			return err
		}
		authority.Lease = lease
		return s.config.NativeRunEvents.ApplyNativeLeaseMutation(c.Request().Context(), w, authority)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLease(c, scope, tracker.Lease{LeaseSummary: tracker.LeaseSummary{ID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Machine: tracker.MachineSummary{ID: tracker.MachineID(lease.MachineID)}, SessionID: lease.SessionID, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt}})
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
	err = s.config.TenantRepository.WriteTenant(c.Request().Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		lease, err := w.Lease(c.Request().Context(), leaseID)
		if err != nil {
			return err
		}
		authority := NativeLeaseAuthority{Scope: scope, Lease: lease, Request: request, Operation: "release"}
		if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(c.Request().Context(), w, &authority); err != nil {
			return err
		}
		if prepared.attempt.ID != "" {
			attempt, err := w.LatestAttempt(c.Request().Context(), lease.IssueID)
			if err != nil {
				return err
			}
			if attempt.ID != prepared.attempt.ID || attempt.LeaseID != prepared.attempt.LeaseID || attempt.FencingToken != prepared.attempt.FencingToken || attempt.Sequence != prepared.attempt.Sequence || attempt.Status != "running" {
				return leasestore.ErrAttemptConflict
			}
			workItem, err := w.WorkItem(c.Request().Context(), scope.project, prepared.workItem.NativeID)
			if err != nil {
				return err
			}
			if workItem.ID != lease.IssueID {
				return corestore.ErrNotFound
			}
			attemptEvent := leasestore.AttemptEvent{ID: attempt.ID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: attempt.RunID,
				Sequence: attempt.Sequence + 1, Status: "cancelled", DispatchGeneration: attempt.DispatchGeneration, PayloadObjectKey: prepared.payloadObjectKey}
			runAuthority := NativeRunEventAuthority{Scope: scope, OrganizationID: scope.organization, ProjectID: scope.project, WorkItem: workItem, Lease: lease, Event: prepared.event}
			if err := s.config.NativeRunEvents.ValidateNativeRunEvent(c.Request().Context(), w, &runAuthority); err != nil {
				return err
			}
			attemptEvent.DispatchGeneration = runAuthority.DispatchGeneration
			stored, duplicate, err := w.RecordAttemptEvent(c.Request().Context(), attemptEvent)
			if err != nil {
				return err
			}
			if duplicate || stored.ID != attempt.ID || stored.Sequence != attemptEvent.Sequence || stored.Status != "cancelled" {
				return leasestore.ErrAttemptConflict
			}
			if _, err := w.AppendEvent(c.Request().Context(), prepared.history); err != nil {
				return err
			}
			if err := s.config.NativeRunEvents.ApplyNativeRunEvent(c.Request().Context(), w, runAuthority); err != nil {
				return err
			}
		}
		if _, err := w.Release(c.Request().Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: int64(request.FencingToken)}, request.Reason); err != nil {
			return err
		}
		if err := s.config.NativeRunEvents.ApplyNativeLeaseMutation(c.Request().Context(), w, authority); err != nil {
			return err
		}
		if s.conversations != nil {
			return s.conversations.leaseReleasedTenant(c.Request().Context(), w, leaseID, authority.Lease.FencingToken, request.Reason)
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
	if s.runDetailStore() == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	cancellation := &preparedNativeLeaseCancellation{}
	var lease leasestore.Lease
	var attempt leasestore.Attempt
	var event eventstore.Event
	var workItem corestore.WorkItem
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		lease, err = r.Lease(ctx, leaseID)
		if err != nil {
			return err
		}
		authority := NativeLeaseAuthority{Scope: scope, Lease: lease, Request: request, Operation: "release"}
		if err := s.config.NativeRunEvents.ValidateNativeLeaseMutation(ctx, r, &authority); err != nil {
			return err
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
	data.MachineID, data.SessionID, data.RunnerID = tracker.MachineID(lease.MachineID), lease.SessionID, scope.credential.Runner.RunnerID
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
	*cancellation = preparedNativeLeaseCancellation{attempt: attempt, workItem: workItem, payloadObjectKey: ref.Key, event: prepared, history: history}
	return cancellation, nil
}

func (s *Service) respondNativeLeaseTenant(c echo.Context, scope nativeScope, leaseID string) error {
	if s.config.NativeRunEvents == nil {
		return s.nativeAPIError(c, nativeRunEventStorageUnavailable())
	}
	var lease leasestore.Lease
	var details NativeLeaseResponseDetails
	err := s.config.TenantRepository.ReadTenant(c.Request().Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		lease, err = r.Lease(c.Request().Context(), leaseID)
		if err != nil {
			return err
		}
		details, err = s.config.NativeRunEvents.NativeLeaseResponse(c.Request().Context(), r, scope, lease)
		if err != nil {
			return err
		}
		details.ProviderReservation = nil
		reservation, foundReservation, err := readTenantProviderReservation(c.Request().Context(), r, tracker.LeaseID(lease.LeaseID))
		if err != nil {
			return err
		}
		if foundReservation {
			details.ProviderReservation = &reservation.Reservation
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if details.WorkItemID == "" {
		return s.nativeAPIError(c, corestore.ErrNotFound)
	}
	return c.JSON(http.StatusOK, tracker.NativeLease{RunDetailBatching: s.runDetailStore() != nil, IsolationPolicy: details.IsolationPolicy,
		ProviderReservation: details.ProviderReservation, ServerTime: s.config.now().UTC(), PolicyID: details.PolicyID,
		ID: tracker.LeaseID(lease.LeaseID), WorkItemID: details.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID,
		FencingToken: tracker.FencingToken(lease.FencingToken), AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt})
}
