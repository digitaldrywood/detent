package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) HealthAttemptSources(ctx context.Context, project tracker.ProjectID, ids []string) (healthstore.AttemptSources, error) {
	result := healthstore.AttemptSources{Attempts: map[string]leasestore.Attempt{}, Metadata: tenantstore.AttemptMetadata{
		Leases: map[string]leasestore.Lease{}, Events: map[string]eventstore.Event{}, Archives: map[string]artifactstore.ArchiveObject{}, Attachments: map[string]attachmentstore.Record{},
	}}
	if project == "" || len(ids) > corestore.MaxPageSize {
		return result, corestore.ErrInvalidPage
	}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.db.Query(ctx, `SELECT a.* FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.project_id=$2 AND a.id=ANY($3::text[]) ORDER BY a.id`, s.organization, project, ids)
	if err != nil {
		return result, mapError(ctx, err)
	}
	attempts, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.NativeAttempt])
	if err != nil {
		return result, mapError(ctx, err)
	}
	var requested, leases, keys, attachments []string
	for _, row := range attempts {
		attempt := attemptFromPG(row)
		result.Attempts[attempt.ID] = attempt
		requested, leases, keys = append(requested, attempt.ID), append(leases, attempt.LeaseID), append(keys, attempt.PayloadObjectKey)
		attachments = append(attachments, "att_"+artifact.Digest([]byte(string(s.organization) + "\n" + attempt.PayloadObjectKey))[:32])
	}
	if len(attempts) == 0 {
		return result, nil
	}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT * FROM leases WHERE organization_id=$1 AND lease_id=ANY($2::text[])`, s.organization, leases).Query(func(rows pgx.Rows) error {
		values, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.Lease])
		for _, row := range values {
			result.Metadata.Leases[row.LeaseID] = leasestore.Lease(row)
		}
		return err
	})
	batch.Queue(`SELECT latest.* FROM unnest($3::text[]) requested(attempt_id)
 JOIN native_attempts a ON a.organization_id=$1 AND a.id=requested.attempt_id
 JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=$2
 CROSS JOIN LATERAL (SELECT * FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=i.native_id AND attempt_id=requested.attempt_id AND attempt_id<>'' ORDER BY sequence DESC LIMIT 1) latest`, s.organization, project, requested).Query(func(rows pgx.Rows) error {
		values, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.CollaborationEvent])
		for _, row := range values {
			result.Metadata.Events[row.AttemptID] = eventRecord(row)
		}
		return err
	})
	batch.Queue(`SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM archive_objects WHERE organization_id=$1 AND object_key=ANY($2::text[])`, s.organization, keys).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var object artifactstore.ArchiveObject
			ref := &object.Reference
			if err := rows.Scan(&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt); err != nil {
				return err
			}
			result.Metadata.Archives[ref.Key] = object
		}
		return rows.Err()
	})
	batch.Queue(`SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND id=ANY($3::text[])`, s.organization, project, attachments).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			row, err := scanAttachmentRecord(rows)
			if err != nil {
				return err
			}
			result.Metadata.Attachments[row.Metadata.ID] = row
		}
		return rows.Err()
	})
	timed, ok := s.db.(timedQueries)
	if !ok {
		return result, corestore.ErrInvalidPage
	}
	executor, ok := timed.DBTX.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		return result, corestore.ErrInvalidPage
	}
	started := time.Now()
	err = executor.SendBatch(ctx, batch).Close()
	for _, query := range batch.QueuedQueries {
		observeStatement(ctx, started, query.SQL)
	}
	return result, mapError(ctx, errors.Join(err, ctx.Err()))
}
