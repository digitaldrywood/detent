package workspacehub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type Deps struct {
	Repository        tenantstore.Repository
	Organization      func() tracker.OrganizationID
	Now               func() time.Time
	Reader            func(tenantstore.Reader) (Reader, error)
	Writer            func(tenantstore.Writer) (Writer, error)
	PrepareIssueClose func(context.Context, tracker.OrganizationID, tracker.ProjectID, string, time.Time) (func(context.Context, tenantstore.Writer) error, error)
	Committed         func(context.Context, Record)
}
type Service struct {
	deps      Deps
	scanMu    sync.Mutex
	scanAfter time.Time
	scanID    string
}

func New(deps Deps) *Service { return &Service{deps: deps} }

const SweepBatch = 100

var errWorkspaceUnchanged = errors.New("workspace changed under the sweep")

func (w *Service) Page(ctx context.Context, after time.Time, id string) ([]Record, error) {
	var records []Record
	err := w.deps.Repository.ReadTenant(ctx, w.deps.Organization(), corestore.Background, func(r tenantstore.Reader) error {
		owner, err := w.deps.Reader(r)
		if err != nil {
			return err
		}
		rows, err := owner.WorkspaceOpenSessions(ctx, after, id, SweepBatch)
		if err != nil {
			return err
		}
		for _, row := range rows {
			records = append(records, TenantRecord(row))
		}
		return nil
	})
	return records, err
}
func (w *Service) Due(ctx context.Context, now time.Time) ([]Record, error) {
	w.scanMu.Lock()
	after, id := w.scanAfter, w.scanID
	w.scanMu.Unlock()
	records, err := w.Page(ctx, after, id)
	if err != nil {
		return nil, err
	}
	w.scanMu.Lock()
	if len(records) < SweepBatch {
		w.scanAfter, w.scanID = time.Time{}, ""
	} else {
		last := records[len(records)-1]
		w.scanAfter, w.scanID = last.ExpiresAt, last.ID
	}
	w.scanMu.Unlock()
	return records, nil
}
func (w *Service) ApplyDeadline(ctx context.Context, record Record, now time.Time) (Record, bool, error) {
	return w.ApplyLifecycle(ctx, record, now, false)
}
func (w *Service) ApplyLifecycle(ctx context.Context, record Record, now time.Time, recovery bool) (Record, bool, error) {

	decision := DecideDeadline(record, now)
	var itemID string
	var current Row
	var checkedLease leasestore.Lease
	var leaseChecked, leaseMissing bool
	err := w.deps.Repository.ReadTenant(ctx, record.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		owner, err := w.deps.Reader(r)
		if err != nil {
			return err
		}
		current, err = owner.WorkspaceSession(ctx, record.ProjectID, record.ID)
		if err != nil {
			return err
		}
		if current.Revision != record.Revision || workspacesession.Terminal(current.State) {
			return errWorkspaceUnchanged
		}
		if recovery {
			if current.State == workspacesession.StateRequested && current.RebindDeadline == nil {
				return errWorkspaceUnchanged
			}
			decision = Deadline{State: workspacesession.StateRequested, Reason: workspacesession.ReasonHubRestarted, EndedAt: now, Apply: true}
			if current.State == workspacesession.StateClosing {
				decision.State, decision.Reason = workspacesession.StateClosed, current.Reason
			}
		}
		if current.LeaseID != "" && current.State != workspacesession.StateRequested && current.State != workspacesession.StateClosing {
			lease, err := r.Lease(ctx, current.LeaseID)
			leaseChecked = true
			checkedLease = lease
			leaseMissing = errors.Is(err, corestore.ErrNotFound)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if errors.Is(err, corestore.ErrNotFound) || lease.FencingToken != current.FencingToken || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
				ended := now
				if err == nil && !lease.ExpiresAt.After(now) {
					ended = lease.ExpiresAt
				}
				if err == nil && lease.ReleasedAt != nil && lease.ReleasedAt.Before(ended) {
					ended = *lease.ReleasedAt
				}
				decision = Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonLeaseLost, EndedAt: ended, Apply: true}
			}
		}
		if !decision.Apply {
			return errWorkspaceUnchanged
		}
		if workspacesession.Terminal(decision.State) {
			item, err := owner.WorkspaceDispatchItem(ctx, record.ProjectID, record.ID)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if item.ClosedAt == nil {
				itemID = string(item.WorkItemID)
			}
		}
		return nil
	})
	if errors.Is(err, errWorkspaceUnchanged) {
		return TenantRecord(current), false, nil
	}
	if err != nil {
		return record, false, err
	}
	closure, err := w.deps.PrepareIssueClose(ctx, record.OrganizationID, record.ProjectID, itemID, now)
	if err != nil {
		return record, false, err
	}
	moved := record
	err = w.deps.Repository.WriteTenant(ctx, record.OrganizationID, corestore.Background, func(writer tenantstore.Writer) error {
		if err := writer.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := writer.LockWorkItemProject(ctx, record.ProjectID); err != nil {
			return err
		}
		owner, err := w.deps.Writer(writer)
		if err != nil {
			return err
		}
		row, err := owner.WorkspaceSession(ctx, record.ProjectID, record.ID)
		if err != nil {
			return err
		}
		if row.Revision != current.Revision || row.RunnerID != current.RunnerID || row.LeaseID != current.LeaseID || row.FencingToken != current.FencingToken {
			return errWorkspaceUnchanged
		}
		if leaseChecked {
			lease, err := writer.Lease(ctx, row.LeaseID)
			missing := errors.Is(err, corestore.ErrNotFound)
			if err != nil && !missing {
				return err
			}
			if missing != leaseMissing || !missing && (lease.FencingToken != checkedLease.FencingToken || !lease.ExpiresAt.Equal(checkedLease.ExpiresAt) || !lease.RenewedAt.Equal(checkedLease.RenewedAt) || !SameTime(lease.ReleasedAt, checkedLease.ReleasedAt)) {
				return errWorkspaceUnchanged
			}
		}
		if itemID != "" {
			item, err := owner.WorkspaceDispatchItem(ctx, record.ProjectID, record.ID)
			if err != nil {
				return err
			}
			if item.ClosedAt != nil || string(item.WorkItemID) != itemID {
				return errWorkspaceUnchanged
			}
			if err := closure(ctx, writer); err != nil {
				return err
			}
			if err := owner.CloseWorkspaceItem(ctx, record.ProjectID, item.WorkItemID, now); err != nil {
				return err
			}
		}
		if recovery && decision.State == workspacesession.StateRequested {
			deadline := now.Add(workspacesession.RebindGrace)
			row.RebindDeadline = &deadline
			row.Capabilities = nil
		}
		row, err = UpdateState(ctx, owner, row, row, decision.State, decision.Reason, now)
		if err != nil {
			return err
		}

		if workspacesession.Terminal(decision.State) && row.RunnerID != "" {
			ended := decision.EndedAt
			if err := owner.EndWorkspaceOccupancy(ctx, record.OrganizationID, record.ProjectID, record.ID, row.RunnerID, ended); err != nil {
				return err
			}
		}
		moved = TenantRecord(row)
		return nil
	})
	if errors.Is(err, errWorkspaceUnchanged) || errors.Is(err, corestore.ErrSerializationFailure) {
		return record, false, nil
	}
	return moved, err == nil, err
}
func (w *Service) RecoverOpen(ctx context.Context) error {
	now := w.deps.Now().UTC()
	var after time.Time
	id := ""
	for {
		records, err := w.Page(ctx, after, id)
		if err != nil {
			return err
		}
		for _, record := range records {
			moved, changed, err := w.ApplyLifecycle(ctx, record, now, true)
			if err != nil {
				return fmt.Errorf("recover workspace %s: %w", record.ID, err)
			}
			if changed {
				w.deps.Committed(ctx, moved)
			}
		}
		if len(records) < SweepBatch {
			return nil
		}
		last := records[len(records)-1]
		after, id = last.ExpiresAt, last.ID
	}
}
