package githubhosted

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Import struct {
	EditSequence    int64            `json:"edit_sequence,string"`
	IntakePending   bool             `json:"intake_pending"`
	ID              string           `json:"import_id"`
	IssueNumber     int              `json:"issue_number"`
	WorkItemID      string           `json:"work_item_id,omitempty"`
	Stage           string           `json:"stage"`
	Cursor          string           `json:"cursor,omitempty"`
	Revision        tracker.Revision `json:"revision,string"`
	Pages           int              `json:"pages"`
	Status          string           `json:"status"`
	Gaps            []string         `json:"gaps"`
	LastError       string           `json:"last_error,omitempty"`
	RetryAfter      *time.Time       `json:"retry_after,omitempty"`
	SourceUpdatedAt string           `json:"source_updated_at,omitempty"`
	ObservedAt      string           `json:"observed_at"`
}

type ImportRecords struct {
	Repository tenantstore.Repository
	Details    func() attachment.DetailStore
}

func ImportResponse(row projectstore.GitHubImport) Import {
	result := Import{ID: row.ID, IssueNumber: int(row.IssueNumber), WorkItemID: row.WorkItemID, Stage: row.Stage, Cursor: row.Cursor, Revision: tracker.Revision(row.Revision), Pages: int(row.Pages), Status: row.Status, Gaps: row.Gaps, LastError: row.LastError, RetryAfter: row.RetryAfter, ObservedAt: hubapi.FormatHubTime(row.ObservedAt), IntakePending: row.IntakePending, EditSequence: row.EditSequence}
	if row.SourceUpdatedAt != nil {
		result.SourceUpdatedAt = hubapi.FormatHubTime(*row.SourceUpdatedAt)
	}
	return result
}

func (s ImportRecords) Read(ctx context.Context, scope hubscope.NativeScope, id string) (Import, error) {
	var result projectstore.GitHubImport
	err := s.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		owner, ok := r.(projectstore.GitHubImportReader)
		if !ok {
			return errors.New("GitHub import owner unavailable")
		}
		result, err = owner.GitHubImport(ctx, scope.Project, id)
		return err
	})
	return ImportResponse(result), hubapi.ProjectRecordError(err)
}

func (s ImportRecords) List(ctx context.Context, scope hubscope.NativeScope, id string, after int64, limit int) ([]projectstore.GitHubImportRecord, error) {
	var result []projectstore.GitHubImportRecord
	err := s.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(projectstore.GitHubImportRecordReader)
		if !ok {
			return errors.New("GitHub import record owner unavailable")
		}
		var err error
		result, err = owner.GitHubImportRecords(ctx, scope.Project, id, after, limit)
		return err
	})
	return result, hubapi.ProjectRecordError(err)
}

func (s ImportRecords) Decode(ctx context.Context, row projectstore.GitHubImportRecord) (githubcontract.GitHubImportRecord, error) {
	raw, err := eventstore.NewContent(s.Details()).Receipt(ctx, row.Payload)
	if err != nil {
		return githubcontract.GitHubImportRecord{}, err
	}
	var record githubcontract.GitHubImportRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, err
	}
	if record.SourceKey != row.SourceKey || record.Kind != row.Kind || row.DependencyID != "" && record.DependencyID != row.DependencyID || row.ProvenanceExternalID != "" && record.Provenance.ExternalID != row.ProvenanceExternalID {
		return githubcontract.GitHubImportRecord{}, artifact.ErrIntegrity
	}
	if record.Kind == "dependency" {
		record.CurrentDependency = new(row.CurrentDependency)
	}
	return record, nil
}
