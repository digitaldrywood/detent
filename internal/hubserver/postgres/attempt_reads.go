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
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AttemptMetadata(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, attempts []leasestore.Attempt, evidence bool) (tenantstore.AttemptMetadata, error) {
	metadata := tenantstore.AttemptMetadata{
		Leases: make(map[string]leasestore.Lease), Events: make(map[string]eventstore.Event),
		Archives: make(map[string]artifactstore.ArchiveObject), Attachments: make(map[string]attachmentstore.Record),
		Evidence: make(map[string][]attachmentstore.Record),
	}
	if len(attempts) > corestore.MaxPageSize || project == "" || item == "" {
		return metadata, corestore.ErrInvalidPage
	}
	ids, leases, keys, attachments := []string{}, []string{}, []string{}, []string{}
	for _, attempt := range attempts {
		if attempt.OrganizationID != s.organization {
			return metadata, corestore.ErrTenantMismatch
		}
		ids, leases, keys = append(ids, attempt.ID), append(leases, attempt.LeaseID), append(keys, attempt.PayloadObjectKey)
		attachments = append(attachments, "att_"+artifact.Digest([]byte(string(s.organization) + "\n" + attempt.PayloadObjectKey))[:32])
	}
	if len(attempts) == 0 {
		return metadata, nil
	}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT * FROM leases WHERE organization_id=$1 AND lease_id=ANY($2::text[])`, s.organization, leases).Query(func(rows pgx.Rows) error {
		values, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.Lease])
		for _, value := range values {
			metadata.Leases[value.LeaseID] = leasestore.Lease(value)
		}
		return err
	})
	batch.Queue(`SELECT latest.* FROM unnest($4::text[]) requested(attempt_id) CROSS JOIN LATERAL (
		SELECT * FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3
		AND attempt_id=requested.attempt_id AND attempt_id<>'' ORDER BY sequence DESC LIMIT 1) latest`, s.organization, project, item, ids).Query(func(rows pgx.Rows) error {
		values, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.CollaborationEvent])
		for _, value := range values {
			metadata.Events[value.AttemptID] = eventRecord(value)
		}
		return err
	})
	batch.Queue(`SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at
		FROM archive_objects WHERE organization_id=$1 AND object_key=ANY($2::text[])`, s.organization, keys).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var object artifactstore.ArchiveObject
			ref := &object.Reference
			if err := rows.Scan(&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt); err != nil {
				return err
			}
			metadata.Archives[ref.Key] = object
		}
		return rows.Err()
	})
	batch.Queue(`SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND id=ANY($3::text[])`, s.organization, project, attachments).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			record, err := scanAttachmentRecord(rows)
			if err != nil {
				return err
			}
			metadata.Attachments[record.Metadata.ID] = record
		}
		return rows.Err()
	})
	if evidence {
		batch.Queue(`SELECT records.* FROM unnest($4::text[]) requested(attempt_id) CROSS JOIN LATERAL (
			SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3
			AND evidence_attempt_id=requested.attempt_id ORDER BY created_at,id LIMIT 201) records`, s.organization, project, item, ids).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				record, err := scanAttachmentRecord(rows)
				if err != nil {
					return err
				}
				metadata.Evidence[record.EvidenceAttemptID] = append(metadata.Evidence[record.EvidenceAttemptID], record)
			}
			return rows.Err()
		})
	}
	timed, ok := s.db.(timedQueries)
	if !ok {
		return metadata, corestore.ErrInvalidPage
	}
	executor, ok := timed.DBTX.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		return metadata, corestore.ErrInvalidPage
	}
	started := time.Now()
	err := executor.SendBatch(ctx, batch).Close()
	for _, query := range batch.QueuedQueries {
		observeStatement(ctx, started, query.SQL)
	}
	return metadata, mapError(ctx, errors.Join(err, ctx.Err()))
}
