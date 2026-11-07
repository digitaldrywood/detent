package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *runnerEventSession) Project(ctx context.Context, id tracker.ProjectID) (corestore.Project, error) {
	if id != f.command.Project {
		return f.Writer.Project(ctx, id)
	}
	return project(f.project.value), f.project.err
}
func (f *runnerEventSession) WorkItem(ctx context.Context, p tracker.ProjectID, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	if p != f.command.Project || id != f.command.WorkItem {
		return f.Writer.WorkItem(ctx, p, id)
	}
	return corestore.WorkItem(f.item.value), f.item.err
}
func (f *runnerEventSession) LockWorkItemProject(ctx context.Context, p tracker.ProjectID) error {
	if p != f.command.Project || f.session.readOnly {
		return corestore.ErrTenantMismatch
	}
	return f.project.err
}
func (f *runnerEventSession) LockWorkItem(ctx context.Context, p tracker.ProjectID, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	if f.session.readOnly {
		return corestore.WorkItem{}, corestore.ErrTenantMismatch
	}
	return f.WorkItem(ctx, p, id)
}
func (f *runnerEventSession) Lease(ctx context.Context, id string) (leasestore.Lease, error) {
	if id != f.command.LeaseID {
		return f.Writer.Lease(ctx, id)
	}
	return leasestore.Lease(f.lease.value), f.lease.err
}
func (f *runnerEventSession) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	if f.session.readOnly {
		return leasestore.Lease{}, corestore.ErrTenantMismatch
	}
	return f.Lease(ctx, id)
}
func (f *runnerEventSession) Attempt(ctx context.Context, id string) (leasestore.Attempt, error) {
	if id != f.command.AttemptID {
		return f.Writer.Attempt(ctx, id)
	}
	return attemptFromPG(f.attempt.value), f.attempt.err
}
func (f *runnerEventSession) LatestAttempt(ctx context.Context, id int64) (leasestore.Attempt, error) {
	if id != f.item.value.ID {
		return f.Writer.LatestAttempt(ctx, id)
	}
	return attemptFromPG(f.latest.value), f.latest.err
}
func (f *runnerEventSession) Machine(ctx context.Context, id string) (leasestore.Machine, error) {
	if id != f.lease.value.MachineID {
		return f.Writer.Machine(ctx, id)
	}
	m := f.machine.value
	return leasestore.Machine{OrganizationID: m.OrganizationID, ID: m.ID, Hostname: m.Hostname, DisplayName: m.DisplayName, Capacity: m.Capacity, Version: m.Version, SpriteName: m.SpriteName}, f.machine.err
}
func (f *runnerEventSession) LeasePolicy(ctx context.Context, id tracker.LeaseID) (policystore.LeasePolicy, error) {
	if string(id) != f.command.LeaseID {
		return f.Writer.LeasePolicy(ctx, id)
	}
	return f.pinned.value, f.pinned.err
}
func (f *runnerEventSession) ProjectPolicy(ctx context.Context, p tracker.ProjectID) (policystore.Revision, error) {
	if p != f.command.Project {
		return f.Writer.ProjectPolicy(ctx, p)
	}
	return projectPolicyCurrentRecord(f.policy.value), f.policy.err
}
func (f *runnerEventSession) LatestRunEvent(ctx context.Context, p tracker.ProjectID, id string) (eventstore.Event, error) {
	if p != f.command.Project || id != f.command.AttemptID {
		return f.Writer.LatestRunEvent(ctx, p, id)
	}
	return eventRecord(f.latestEvent.value), f.latestEvent.err
}
func (f *runnerEventSession) RunEvent(_ context.Context, p tracker.ProjectID, id tracker.NativeWorkItemID, key string) (eventstore.Event, error) {
	row, ok := f.events[key]
	if !ok || p != f.command.Project || id != f.command.WorkItem {
		return eventstore.Event{}, corestore.ErrNotFound
	}
	return row, nil
}
func (f *runnerEventSession) RunnerForLease(ctx context.Context, id string) (runnerstore.Identity, error) {
	if id != f.command.LeaseID {
		return f.Writer.RunnerForLease(ctx, id)
	}
	if f.owner == "" {
		return runnerstore.Identity{}, corestore.ErrNotFound
	}
	return f.RunnerIdentity(ctx, f.owner)
}
func (f *runnerEventSession) RunnerSessionTime(context.Context) (time.Time, error) { return f.now, nil }
func (f *runnerEventSession) RunnerSession(ctx context.Context, p tracker.ProjectID, id int64) (runnersession.Record, error) {
	if p != f.command.Project || id != f.command.SessionID {
		return f.sessions.RunnerSession(ctx, p, id)
	}
	return f.sessionRecord.value, f.sessionRecord.err
}
func (f *runnerEventSession) IssueDispatchState(_ context.Context, p tracker.ProjectID, id tracker.NativeWorkItemID) (tenantstore.DispatchState, error) {
	if p != f.command.Project || id != f.command.WorkItem {
		return tenantstore.DispatchState{}, corestore.ErrTenantMismatch
	}
	return tenantstore.DispatchState{Revision: f.item.value.Revision, Generation: f.item.value.DispatchGeneration, RequestedAt: f.item.value.DispatchRequestedAt}, f.item.err
}

func (f *runnerEventSession) Receipt(ctx context.Context, actor, operation, key string) (eventstore.Receipt, error) {
	if actor != f.command.ActorID || operation != f.command.Operation || key != f.command.Key {
		return f.Writer.Receipt(ctx, actor, operation, key)
	}
	return f.receipt.value, f.receipt.err
}

func (f *runnerEventSession) RunnerProjectAuthorized(_ context.Context, runner string, p tracker.ProjectID, _ time.Time) (bool, error) {
	if runner != f.owner || p != f.command.Project {
		return false, corestore.ErrTenantMismatch
	}
	return f.runnerAllowed, nil
}
