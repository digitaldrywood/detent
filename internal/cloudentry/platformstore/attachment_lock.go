package platformstore

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (r *Repository) LockAttachment(ctx context.Context, workload corestore.Workload, key string) (*EntryTransaction, error) {
	tx, err := r.BeginEntry(ctx, workload, true)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('attachment:' || $1::text,0))`, key); err != nil {
		return nil, errors.Join(err, tx.Rollback(ctx))
	}
	return tx, nil
}

func (r *Repository) LockAttachmentForSweep(ctx context.Context, key, organization, at string) (unlock func() error, audit func() error, err error) {
	release, err := r.acquireEntry(ctx, corestore.Serving)
	if err != nil {
		return nil, nil, err
	}
	tx, err := r.LockAttachment(ctx, corestore.Background, key)
	if err != nil {
		release()
		return nil, nil, err
	}
	return func() error { defer release(); return tx.Rollback(ctx) }, func() error {
		_, err := r.pool.Exec(ctx, `INSERT INTO platform.audit(subject,organization_id,event,recorded_at) VALUES ('entry',$1,'attachment_deleted',$2)`, organization, at)
		return err
	}, nil
}
