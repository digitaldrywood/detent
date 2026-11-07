package postgres

import (
	"context"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
)

func (f *runnerEventSession) AdjustCollaborationBytes(_ context.Context, delta, limit int64) (billingstore.UsageResult, error) {
	if delta < 0 || limit != math.MaxInt64 {
		return billingstore.UsageResult{}, billingstore.ErrInvalidCollaborationBytes
	}
	f.pending.Queue(`UPDATE hosted_usage_counters SET value=value+$2::bigint WHERE organization_id=$1 AND name='collaboration_bytes' AND value>=0 AND value<=$3::bigint-$2::bigint RETURNING value`, f.session.organization, delta, limit).QueryRow(func(row pgx.Row) error {
		var value int64
		return row.Scan(&value)
	})
	return billingstore.UsageResult{Allowed: true}, nil
}

func (f *runnerEventSession) CreateComment(ctx context.Context, record corestore.Comment) (corestore.Comment, error) {
	if record.OrganizationID != f.session.organization || record.ProjectID != f.command.Project || record.WorkItemID != f.command.WorkItem {
		return corestore.Comment{}, corestore.ErrTenantMismatch
	}
	s := &session{organization: f.session.organization, queries: pgquery.New(runEventQueue{f.pending})}
	if _, err := s.CreateComment(ctx, record); err != nil {
		return corestore.Comment{}, err
	}
	return record, nil
}

func (f *runnerEventSession) CreateCollaborationVersion(ctx context.Context, record eventstore.CollaborationVersion) (eventstore.CollaborationVersion, error) {
	if record.OrganizationID != f.session.organization || record.ProjectID != f.command.Project || record.WorkItemID != f.command.WorkItem {
		return eventstore.CollaborationVersion{}, corestore.ErrTenantMismatch
	}
	if record.RecordID == "" || record.Revision != 1 {
		return eventstore.CollaborationVersion{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.CollaborationVersion{}, err
	}
	s := &session{organization: f.session.organization, queries: pgquery.New(runEventQueue{f.pending})}
	if _, err := s.insertRecordCollaborationVersion(ctx, record); err != nil {
		return eventstore.CollaborationVersion{}, err
	}
	return record, nil
}

type runEventAffectedQueue struct{ runEventQueue }

func (q runEventAffectedQueue) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.batch.Queue(sql, args...).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() != 1 {
			return corestore.ErrRevisionConflict
		}
		return nil
	})
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (f *runnerEventSession) PutSearch(ctx context.Context, document searchstore.Document, expected int64) error {
	if document.OrganizationID != f.session.organization || document.ProjectID != f.command.Project || document.WorkItemID != f.command.WorkItem || document.Kind != "comment" || expected != 0 {
		return corestore.ErrTenantMismatch
	}
	s := &session{organization: f.session.organization, queries: pgquery.New(runEventAffectedQueue{runEventQueue{f.pending}})}
	return s.PutSearch(ctx, document, expected)
}
