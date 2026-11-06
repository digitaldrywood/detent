package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func importRecord(row pgquery.GithubImportRecord) projectstore.GitHubImportRecord {
	return projectstore.GitHubImportRecord{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ImportID: row.ImportID, Sequence: row.Sequence, SourceKey: row.SourceKey, Kind: row.Kind, CurrentDependency: row.CurrentDependency, DependencyID: row.DependencyID, ProvenanceExternalID: row.ProvenanceExternalID, Payload: eventstore.Receipt{OrganizationID: row.OrganizationID, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}}
}

func (s *session) GitHubImportRecords(ctx context.Context, project tracker.ProjectID, id string, after int64, limit int) ([]projectstore.GitHubImportRecord, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListGitHubImportRecords(ctx, pgquery.ListGitHubImportRecordsParams{OrganizationID: s.organization, ProjectID: project, ImportID: id, Sequence: after, Limit: int32(limit + 1)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]projectstore.GitHubImportRecord, len(rows))
	for i, row := range rows {
		result[i] = importRecord(row)
	}
	return result, nil
}

func (s *session) GitHubImportEditSource(ctx context.Context, project tracker.ProjectID, id string, after int64) (projectstore.GitHubImportRecord, error) {
	row, err := s.queries.ReadGitHubImportEditSource(ctx, pgquery.ReadGitHubImportEditSourceParams{OrganizationID: s.organization, ProjectID: project, ImportID: id, Sequence: after})
	return importRecord(row), mapError(ctx, err)
}

func (s *session) CreateGitHubImportRecord(ctx context.Context, record projectstore.GitHubImportRecord) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	if record.Payload.OrganizationID != s.organization || record.SourceKey == "" || record.Kind == "" {
		return eventstore.ErrInvalidRecord
	}
	ref := record.Payload
	if err := validRecordPayload(s.organization, ref.PayloadObjectKey, ref.PayloadSHA256, ref.PayloadBytes, ref.PayloadContentBytes); err != nil {
		return err
	}
	return mapError(ctx, s.queries.CreateGitHubImportRecord(ctx, pgquery.CreateGitHubImportRecordParams{OrganizationID: s.organization, ProjectID: record.ProjectID, ImportID: record.ImportID, SourceKey: record.SourceKey, Kind: record.Kind, CurrentDependency: record.CurrentDependency, DependencyID: record.DependencyID, ProvenanceExternalID: record.ProvenanceExternalID, PayloadObjectKey: ref.PayloadObjectKey, PayloadSHA256: ref.PayloadSHA256, PayloadBytes: ref.PayloadBytes, PayloadContentBytes: ref.PayloadContentBytes}))
}
