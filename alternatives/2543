package postgres

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func linkedIssueSourceRecord(row pgquery.LinkedIssueSource) eventstore.LinkedSource {
	result := eventstore.LinkedSource{
		OrganizationID: row.OrganizationID,
		ProjectID:      row.ProjectID,
		WorkItemID:     tracker.NativeWorkItemID(row.WorkItemID),
		URL:            row.SourceUrl,
		TitleSupplied:  row.TitleSupplied,
		BodySupplied:   row.BodySupplied,
		TitleOwned:     row.TitleOwned,
		BodyOwned:      row.BodyOwned,
	}
	if row.SnapshotObjectKey != nil {
		result.SnapshotObjectKey = *row.SnapshotObjectKey
	}
	if row.SnapshotSha256 != nil {
		result.SnapshotSHA256 = *row.SnapshotSha256
	}
	if row.SnapshotBytes.Valid {
		result.SnapshotBytes = row.SnapshotBytes.Int64
	}
	if row.SnapshotContentBytes.Valid {
		result.SnapshotContentBytes = row.SnapshotContentBytes.Int64
	}
	return result
}

func linkedIssueSourcePayload(record eventstore.LinkedSource) (*string, *string, sql.NullInt64, sql.NullInt64, error) {
	if record.SnapshotObjectKey == "" && record.SnapshotSHA256 == "" && record.SnapshotBytes == 0 && record.SnapshotContentBytes == 0 {
		return nil, nil, sql.NullInt64{}, sql.NullInt64{}, nil
	}
	if !artifact.ValidHash(record.SnapshotSHA256, 64) || record.SnapshotBytes <= 0 || record.SnapshotContentBytes <= 0 {
		return nil, nil, sql.NullInt64{}, sql.NullInt64{}, eventstore.ErrInvalidRecord
	}
	ref := eventstore.Reference(record.SnapshotObjectKey, record.SnapshotSHA256, record.SnapshotBytes, record.SnapshotContentBytes)
	if ref.Validate(string(record.OrganizationID)) != nil {
		return nil, nil, sql.NullInt64{}, sql.NullInt64{}, eventstore.ErrInvalidRecord
	}
	return new(record.SnapshotObjectKey), new(record.SnapshotSHA256), sql.NullInt64{Int64: record.SnapshotBytes, Valid: true}, sql.NullInt64{Int64: record.SnapshotContentBytes, Valid: true}, nil
}

func (s *session) LinkedSource(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.LinkedSource, error) {
	row, err := s.queries.RecordGetLinkedIssueSource(ctx, pgquery.RecordGetLinkedIssueSourceParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
	return linkedIssueSourceRecord(row), mapError(ctx, err)
}

func (s *session) LinkedSourceByURL(ctx context.Context, project tracker.ProjectID, sourceURL string) (eventstore.LinkedSource, error) {
	row, err := s.queries.RecordGetLinkedIssueSourceByURL(ctx, pgquery.RecordGetLinkedIssueSourceByURLParams{OrganizationID: s.organization, ProjectID: project, SourceUrl: sourceURL})
	return linkedIssueSourceRecord(row), mapError(ctx, err)
}

func (s *session) CreateLinkedSource(ctx context.Context, record eventstore.LinkedSource) (eventstore.LinkedSource, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.LinkedSource{}, err
	}
	if record.ProjectID == "" || record.WorkItemID == "" || record.URL == "" {
		return eventstore.LinkedSource{}, eventstore.ErrInvalidRecord
	}
	key, digest, bytes, contentBytes, err := linkedIssueSourcePayload(record)
	if err != nil {
		return eventstore.LinkedSource{}, err
	}
	row, err := s.queries.RecordInsertLinkedIssueSource(ctx, pgquery.RecordInsertLinkedIssueSourceParams{OrganizationID: s.organization, ProjectID: record.ProjectID, WorkItemID: string(record.WorkItemID), SourceUrl: record.URL, TitleSupplied: record.TitleSupplied, BodySupplied: record.BodySupplied, TitleOwned: record.TitleOwned, BodyOwned: record.BodyOwned, SnapshotObjectKey: key, SnapshotSha256: digest, SnapshotBytes: bytes, SnapshotContentBytes: contentBytes})
	return linkedIssueSourceRecord(row), mapError(ctx, err)
}

func (s *session) UpdateLinkedSource(ctx context.Context, record eventstore.LinkedSource) (eventstore.LinkedSource, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.LinkedSource{}, err
	}
	key, digest, bytes, contentBytes, err := linkedIssueSourcePayload(record)
	if err != nil {
		return eventstore.LinkedSource{}, err
	}
	row, err := s.queries.RecordUpdateLinkedIssueSource(ctx, pgquery.RecordUpdateLinkedIssueSourceParams{OrganizationID: s.organization, ProjectID: record.ProjectID, WorkItemID: string(record.WorkItemID), SourceUrl: record.URL, TitleOwned: record.TitleOwned, BodyOwned: record.BodyOwned, SnapshotObjectKey: key, SnapshotSha256: digest, SnapshotBytes: bytes, SnapshotContentBytes: contentBytes, ExpectedSnapshotSha256: record.ExpectedSnapshotSHA256})
	return linkedIssueSourceRecord(row), updated(ctx, err)
}
