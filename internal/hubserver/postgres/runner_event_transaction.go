package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func (s *session) WithRunnerEventFacts(ctx context.Context, command eventstore.RunCommand, use func(tenantstore.Writer) error) error {
	f, facts := newRunnerEventFacts(s, s, command)
	b := new(pgx.Batch)
	if !s.readOnly {
		b.Queue(`SELECT r.runner_id FROM runner_identities r
JOIN runner_lease_assignments a ON a.organization_id=r.organization_id AND a.runner_id=r.runner_id
WHERE a.organization_id=$1 AND a.lease_id=$2 FOR UPDATE OF r`, s.organization, command.LeaseID)
	}
	b.QueuedQueries = append(b.QueuedQueries, facts.QueuedQueries...)
	db, ok := s.db.(timedQueries)
	if !ok {
		return corestore.ErrInvalidPage
	}
	executor, ok := db.DBTX.(pgtransport.Executor)
	if !ok {
		return corestore.ErrInvalidPage
	}
	started := time.Now()
	batchErr := executor.SendBatch(ctx, b).Close()
	for _, q := range b.QueuedQueries {
		observeStatement(ctx, started, q.SQL)
	}
	if err := batchErr; err != nil {
		return mapError(ctx, err)
	}

	f.command.WorkItem = f.item.value.NativeID
	if f.command.AttemptID == "" {
		f.command.AttemptID = f.attempt.value.ID
	}
	if err := use(f); err != nil {
		return err
	}
	return f.flushRunEvents(ctx)
}

func (f *runnerEventSession) queueRunFence(cleanup bool) {
	f.pending.Queue(`SELECT EXISTS(SELECT 1 FROM leases WHERE organization_id=$1 AND lease_id=$2 AND fencing_token=$3 AND (($4 AND (released_at IS NOT NULL OR expires_at<=clock_timestamp())) OR (NOT $4 AND released_at IS NULL AND expires_at>clock_timestamp())))`, f.session.organization, f.command.LeaseID, f.lease.value.FencingToken, cleanup).QueryRow(func(row pgx.Row) error {
		var valid bool
		if err := row.Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return &leasestore.FenceError{Check: "attempt_live_lease"}
		}
		return nil
	})
}
