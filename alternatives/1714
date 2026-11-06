package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func importJobResponse(row projectstore.GitHubImport) GitHubImport {
	result := GitHubImport{ID: row.ID, IssueNumber: int(row.IssueNumber), WorkItemID: row.WorkItemID, Stage: row.Stage, Cursor: row.Cursor, Revision: tracker.Revision(row.Revision), Pages: int(row.Pages), Status: row.Status, Gaps: row.Gaps, LastError: row.LastError, RetryAfter: row.RetryAfter, ObservedAt: formatHubTime(row.ObservedAt), IntakePending: row.IntakePending, EditSequence: row.EditSequence}
	if row.SourceUpdatedAt != nil {
		result.SourceUpdatedAt = formatHubTime(*row.SourceUpdatedAt)
	}
	return result
}

func (s *Service) readGitHubImportRecord(ctx context.Context, scope nativeScope, id string) (GitHubImport, error) {
	var result projectstore.GitHubImport
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		owner, ok := r.(projectstore.GitHubImportReader)
		if !ok {
			return errors.New("GitHub import owner unavailable")
		}
		result, err = owner.GitHubImport(ctx, scope.project, id)
		return err
	})
	return importJobResponse(result), projectRecordError(err)
}

func (s *Service) readGitHubImportRecords(ctx context.Context, scope nativeScope, id string, after int64, limit int) ([]projectstore.GitHubImportRecord, error) {
	var result []projectstore.GitHubImportRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(projectstore.GitHubImportRecordReader)
		if !ok {
			return errors.New("GitHub import record owner unavailable")
		}
		var err error
		result, err = owner.GitHubImportRecords(ctx, scope.project, id, after, limit)
		return err
	})
	return result, projectRecordError(err)
}

func (s *Service) decodeGitHubImportRecord(ctx context.Context, row projectstore.GitHubImportRecord) (GitHubImportRecord, error) {
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row.Payload)
	if err != nil {
		return GitHubImportRecord{}, err
	}
	var record GitHubImportRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, err
	}
	if record.SourceKey != row.SourceKey || record.Kind != row.Kind || record.DependencyID != row.DependencyID || record.Provenance.ExternalID != row.ProvenanceExternalID {
		return GitHubImportRecord{}, artifact.ErrIntegrity
	}
	if record.Kind == "dependency" {
		record.CurrentDependency = new(row.CurrentDependency)
	}
	return record, nil
}
