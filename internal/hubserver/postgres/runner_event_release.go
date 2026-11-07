package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *runnerEventSession) WorkItemByID(ctx context.Context, project tracker.ProjectID, id int64) (corestore.WorkItem, error) {
	if project == f.command.Project && id == f.item.value.ID {
		return corestore.WorkItem(f.item.value), f.item.err
	}
	return f.Writer.WorkItemByID(ctx, project, id)
}

func (f *runnerEventSession) LeaseMutationFacts(ctx context.Context, project tracker.ProjectID, id string) (leasestore.Lease, corestore.WorkItem, error) {
	if project != f.command.Project || id != f.command.LeaseID {
		return leasestore.Lease{}, corestore.WorkItem{}, corestore.ErrTenantMismatch
	}
	lease, err := f.Lease(ctx, id)
	if err != nil {
		return leasestore.Lease{}, corestore.WorkItem{}, err
	}
	return lease, corestore.WorkItem(f.item.value), f.item.err
}

func (f *runnerEventSession) LeaseMutationIdentity(ctx context.Context, id string) (runnerstore.Identity, error) {
	return f.RunnerForLease(ctx, id)
}

func (f *runnerEventSession) Release(ctx context.Context, fence leasestore.Fence, reason string) (leasestore.Lease, error) {
	queue := runEventReleaseQueue{runEventQueue{f.pending}, f}
	var clock *time.Time
	if f.session.leaseClock != nil {
		clock = new(f.session.leaseClock().UTC())
	}
	_, err := pgquery.New(queue).MutateLeaseCommand(ctx, pgquery.MutateLeaseCommandParams{OrganizationID: f.session.organization, LeaseID: fence.LeaseID, FencingToken: fence.FencingToken, Operation: "release", Reason: reason, LeaseClock: clock})
	if err != nil {
		return leasestore.Lease{}, err
	}
	lease := leasestore.Lease(f.lease.value)
	lease.ReleasedAt, lease.ReleaseReason = &f.now, reason
	f.lease.value = pgquery.Lease(lease)
	return lease, nil
}

type runEventReleaseQueue struct {
	runEventQueue
	facts *runnerEventSession
}
type runEventReleaseRow struct {
	query *pgx.QueuedQuery
	facts *runnerEventSession
}

func (q runEventReleaseQueue) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return runEventReleaseRow{q.batch.Queue(sql, args...), q.facts}
}

func (r runEventReleaseRow) Scan(_ ...any) error {
	r.query.QueryRow(func(row pgx.Row) error {
		var lease pgquery.Lease
		var applied bool
		if err := row.Scan(&lease.OrganizationID, &lease.LeaseID, &lease.FencingToken, &lease.IssueID, &lease.MachineID, &lease.SessionID, &lease.WorkItemRevision, &lease.LandingCapacity, &lease.AcquiredAt, &lease.RenewedAt, &lease.ExpiresAt, &lease.ReleasedAt, &lease.ReleaseReason, &applied); err != nil {
			return err
		}
		if !applied {
			return tracker.ErrStaleFencingToken
		}
		r.facts.lease = runFact[pgquery.Lease]{value: lease}
		return nil
	})
	return nil
}

func (f *runnerEventSession) ConversationByExecutionLease(ctx context.Context, leaseID string) (conversationstore.Conversation, error) {
	record, err := f.Writer.ConversationByExecutionLease(ctx, leaseID)
	if err != nil {
		return conversationstore.Conversation{}, err
	}
	if err := f.flushRunEvents(ctx); err != nil {
		return conversationstore.Conversation{}, err
	}
	return record, nil
}
