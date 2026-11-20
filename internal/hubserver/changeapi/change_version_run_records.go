package changeapi

import (
	"context"
	"errors"
	"reflect"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errNoChangeVersionRun = errors.New("change version has no source run")

type PreparedVersionRun struct {
	authority VersionRunAuthority
	attempt   leasestore.Attempt
	event     eventstore.Event
	lease     leasestore.Lease
	workItem  corestore.WorkItem
	diff      *leasestore.AttemptDiff
	sessionID int64
	machineID string
}

func (s *VersionPublication) PrepareChangeVersionRun(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion) (*PreparedVersionRun, error) {
	if request.AttemptID == "" {
		if scope.Credential.Scope == hubapi.APIScopeWorker {
			return nil, hubapi.NativePublicInvalid("Worker versions must identify their fenced run and attempt")
		}
		return nil, errNoChangeVersionRun
	}
	if s.deps.Repository == nil || !s.deps.RunEventsAvailable() {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	prepared := &PreparedVersionRun{sessionID: -1}
	var event eventstore.Event
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		prepared.workItem, err = r.WorkItem(ctx, scope.Project, change.WorkItemID)
		if err != nil {
			return err
		}
		prepared.attempt, err = r.Attempt(ctx, request.AttemptID)
		if err != nil {
			return err
		}
		publishing := prepared.attempt.Status == "running" && scope.Credential.Scope == hubapi.APIScopeWorker && scope.Credential.Runner.RunnerID != ""
		if prepared.attempt.ID != request.AttemptID || prepared.attempt.IssueID != prepared.workItem.ID || prepared.attempt.RunID != request.RunID {
			return corestore.ErrNotFound
		}
		if prepared.attempt.Status != "succeeded" && !publishing || prepared.attempt.DispatchGeneration < 0 {
			return s.deps.ExecutionConflict("Version source must identify the current publishing or successful attempt for this work item")
		}
		prepared.lease, err = r.Lease(ctx, prepared.attempt.LeaseID)
		if err != nil {
			return err
		}
		if prepared.lease.IssueID != prepared.workItem.ID || prepared.lease.FencingToken != prepared.attempt.FencingToken || prepared.lease.MachineID == "" || prepared.lease.SessionID == "" {
			return s.deps.ExecutionConflict("Version source attempt does not match its fenced lease")
		}
		if scope.Credential.Scope == hubapi.APIScopeWorker && (string(request.LeaseID) != prepared.lease.LeaseID || int64(request.FencingToken) != prepared.lease.FencingToken) {
			return s.deps.ExecutionConflict("Version publication must use the source attempt's fenced lease")
		}
		event, err = r.LatestRunEvent(ctx, scope.Project, request.AttemptID)
		if err != nil {
			return err
		}
		if event.Type != "run.finished" && !publishing || event.AttemptID != request.AttemptID || event.RunID != request.RunID || event.ProjectID != scope.Project || event.WorkItemID != change.WorkItemID {
			return s.deps.ExecutionConflict("Version source must bind to its persisted run event")
		}
		if publishing {
			diff, err := r.LatestAttemptDiff(ctx, scope.Project, request.AttemptID, tracker.DiffSourceAttempt)
			if err != nil {
				return err
			}
			if diff.OrganizationID != scope.Organization || diff.AttemptID != request.AttemptID || diff.WorkItemID != change.WorkItemID || diff.IssueID != prepared.workItem.ID || diff.BaseSHA != request.BaseSHA || diff.HeadSHA != request.HeadSHA || diff.Producer.ID != request.AttemptID || diff.Producer.RunnerID != scope.Credential.Runner.RunnerID || diff.Producer.LeaseID != request.LeaseID || diff.Producer.FencingToken != request.FencingToken {
				return s.deps.ExecutionConflict("Version source must match the publishing attempt's persisted final diff")
			}
			prepared.diff = &diff
		}
		return nil
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	payload, err := eventstore.NewContent(s.deps.RunDetailStore()).Event(ctx, event)
	if err != nil {
		return nil, err
	}
	if payload.Data.Run == nil || payload.Type != event.Type || payload.ProjectID != scope.Project || payload.AggregateID != change.WorkItemID || payload.Data.Run.AttemptID != request.AttemptID || payload.Data.Run.RunID != request.RunID || string(payload.Data.Run.LeaseID) != prepared.lease.LeaseID || int64(payload.Data.Run.FencingToken) != prepared.lease.FencingToken || payload.Data.Run.PolicyID != request.PolicyID || prepared.diff == nil && payload.Data.Run.Outcome != "succeeded" || prepared.diff != nil && payload.Data.Run.Outcome != "" {
		return nil, s.deps.ExecutionConflict("Persisted event does not match the requested version source")
	}
	prepared.event = event
	prepared.authority = VersionRunAuthority{
		UsageCorrection: prepared.attempt.Status == "succeeded",
		Scope:           scope, OrganizationID: scope.Organization, ProjectID: scope.Project,
		WorkItem: prepared.workItem, Lease: prepared.lease, DispatchGeneration: prepared.attempt.DispatchGeneration,
		Event: tracker.NativeRunEvent{Type: event.Type, SchemaVersion: int(event.SchemaVersion), OccurredAt: event.RecordedAt, Data: *payload.Data.Run},
	}
	prepared.sessionID = nativeImplementingSession(*payload.Data.Run)
	prepared.machineID = string(payload.Data.Run.MachineID)
	if payload.Data.Run.Identity == nil || payload.Data.Run.Identity.Role != "code" && payload.Data.Run.Identity.Role != "rework" && payload.Data.Run.Identity.Role != "merge" || prepared.machineID != prepared.lease.MachineID {
		return nil, s.deps.ExecutionConflict("Version source must retain its coding session and machine attribution")
	}
	return prepared, nil
}

func (s *VersionPublication) recheckChangeVersionRun(ctx context.Context, w tenantstore.Writer, prepared *PreparedVersionRun, identity runnerstore.Identity) error {
	if prepared == nil {
		return nil
	}
	if !s.deps.RunEventsAvailable() {
		return hubapi.NativeRunEventStorageUnavailable()
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
		return hubapi.NativeRunEventStorageUnavailable()
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
	if prepared.diff != nil {
		diff, err := w.LatestAttemptDiff(ctx, prepared.authority.ProjectID, prepared.attempt.ID, tracker.DiffSourceAttempt)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(diff, *prepared.diff) {
			return corestore.ErrRevisionConflict
		}
	}
	authority := prepared.authority
	authority.WorkItem, authority.Lease = workItem, lease
	if authority.Scope.Credential.Scope != hubapi.APIScopeWorker {
		authority.Scope.Credential = hubapi.APICredential{ID: identity.TokenID, Scope: hubapi.APIScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID}}}
	}
	return s.deps.ValidateRunOwner(ctx, w, authority)
}
