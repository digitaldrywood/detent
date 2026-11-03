package hubserver

import (
	"context"
	"errors"
	"reflect"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errNoChangeVersionRun = errors.New("change version has no source run")

type preparedChangeVersionRun struct {
	authority NativeRunEventAuthority
	attempt   leasestore.Attempt
	event     eventstore.Event
	lease     leasestore.Lease
	workItem  corestore.WorkItem
	sessionID int64
	machineID string
}

func (s *Service) prepareChangeVersionRun(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion) (*preparedChangeVersionRun, error) {
	if request.AttemptID == "" {
		if scope.credential.Scope == apiScopeWorker {
			return nil, nativePublicInvalid("Worker versions must identify their fenced run and attempt")
		}
		return nil, errNoChangeVersionRun
	}
	if s.config.TenantRepository == nil || s.config.NativeRunEvents == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	prepared := &preparedChangeVersionRun{sessionID: -1}
	var event eventstore.Event
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		prepared.workItem, err = r.WorkItem(ctx, scope.project, change.WorkItemID)
		if err != nil {
			return err
		}
		prepared.attempt, err = r.Attempt(ctx, request.AttemptID)
		if err != nil {
			return err
		}
		if prepared.attempt.ID != request.AttemptID || prepared.attempt.IssueID != prepared.workItem.ID || prepared.attempt.RunID != request.RunID || prepared.attempt.Status != "succeeded" || prepared.attempt.DispatchGeneration < 0 {
			return nativeExecutionConflict("Version source must identify the successful attempt for this work item")
		}
		prepared.lease, err = r.Lease(ctx, prepared.attempt.LeaseID)
		if err != nil {
			return err
		}
		if prepared.lease.IssueID != prepared.workItem.ID || prepared.lease.FencingToken != prepared.attempt.FencingToken || prepared.lease.MachineID == "" || prepared.lease.SessionID == "" {
			return nativeExecutionConflict("Version source attempt does not match its fenced lease")
		}
		if scope.credential.Scope == apiScopeWorker && (string(request.LeaseID) != prepared.lease.LeaseID || int64(request.FencingToken) != prepared.lease.FencingToken) {
			return nativeExecutionConflict("Version publication must use the source attempt's fenced lease")
		}
		event, err = r.LatestRunEvent(ctx, scope.project, request.AttemptID)
		if err != nil {
			return err
		}
		if event.Type != "run.finished" || event.AttemptID != request.AttemptID || event.RunID != request.RunID || event.ProjectID != scope.project || event.WorkItemID != change.WorkItemID {
			return nativeExecutionConflict("Version source must bind to the persisted final run event")
		}
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	payload, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, event)
	if err != nil {
		return nil, err
	}
	if payload.Data.Run == nil || payload.Type != "run.finished" || payload.ProjectID != scope.project || payload.AggregateID != change.WorkItemID || payload.Data.Run.AttemptID != request.AttemptID || payload.Data.Run.RunID != request.RunID || string(payload.Data.Run.LeaseID) != prepared.lease.LeaseID || int64(payload.Data.Run.FencingToken) != prepared.lease.FencingToken || payload.Data.Run.PolicyID != request.PolicyID || payload.Data.Run.Outcome != "succeeded" {
		return nil, nativeExecutionConflict("Persisted final event does not match the requested version source")
	}
	prepared.event = event
	prepared.authority = NativeRunEventAuthority{
		Scope: scope, OrganizationID: scope.organization, ProjectID: scope.project,
		WorkItem: prepared.workItem, Lease: prepared.lease, DispatchGeneration: prepared.attempt.DispatchGeneration,
		Event: tracker.NativeRunEvent{Type: event.Type, SchemaVersion: int(event.SchemaVersion), OccurredAt: event.RecordedAt, Data: *payload.Data.Run},
	}
	prepared.sessionID = nativeImplementingSession(*payload.Data.Run)
	prepared.machineID = string(payload.Data.Run.MachineID)
	if payload.Data.Run.Identity == nil || payload.Data.Run.Identity.Role != "code" && payload.Data.Run.Identity.Role != "rework" || prepared.machineID != prepared.lease.MachineID {
		return nil, nativeExecutionConflict("Version source must retain its coding session and machine attribution")
	}
	return prepared, nil
}

func (s *Service) recheckChangeVersionRun(ctx context.Context, w tenantstore.Writer, prepared *preparedChangeVersionRun) error {
	if prepared == nil {
		return nil
	}
	if s.config.NativeRunEvents == nil {
		return nativeRunEventStorageUnavailable()
	}
	workItem, err := w.WorkItem(ctx, prepared.authority.ProjectID, prepared.authority.WorkItem.NativeID)
	if err != nil {
		return err
	}
	attempt, err := w.Attempt(ctx, prepared.attempt.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(attempt, prepared.attempt) || workItem.ID != prepared.workItem.ID || workItem.Revision != prepared.workItem.Revision {
		return corestore.ErrRevisionConflict
	}
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	lease, err := locker.LockLease(ctx, prepared.lease.LeaseID)
	if err != nil {
		return err
	}
	if lease.LeaseID != prepared.lease.LeaseID || lease.IssueID != workItem.ID || lease.FencingToken != prepared.lease.FencingToken || lease.MachineID != prepared.lease.MachineID || lease.SessionID != prepared.lease.SessionID {
		return tracker.ErrStaleFencingToken
	}
	latest, err := w.LatestRunEvent(ctx, prepared.authority.ProjectID, prepared.attempt.ID)
	if err != nil {
		return err
	}
	if latest.ID != prepared.event.ID || latest.Sequence != prepared.event.Sequence || latest.PayloadSHA256 != prepared.event.PayloadSHA256 {
		return corestore.ErrRevisionConflict
	}
	authority := prepared.authority
	authority.WorkItem, authority.Lease = workItem, lease
	return (tenantNativeRunEventParticipant{service: s}).validateNativeRunEventOwner(ctx, w, &authority)
}
