package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func importJob(row pgquery.GithubImport) projectstore.GitHubImport {
	return projectstore.GitHubImport{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, IssueNumber: row.IssueNumber, WorkItemID: row.WorkItemID, Stage: row.Stage, Cursor: row.Cursor, Revision: row.Revision, Pages: row.Pages, Status: row.Status, Gaps: row.Gaps, LastError: row.LastError, RetryAfter: row.RetryAfter, SourceUpdatedAt: row.SourceUpdatedAt, ObservedAt: row.ObservedAt, IntakePending: row.IntakePending, EditSequence: row.EditSequence}
}

func (s *session) GitHubImport(ctx context.Context, project tracker.ProjectID, id string) (projectstore.GitHubImport, error) {
	row, err := s.queries.ReadGitHubImport(ctx, pgquery.ReadGitHubImportParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return importJob(row), mapError(ctx, err)
}

func (s *session) CreateGitHubImport(ctx context.Context, record projectstore.GitHubImport) (projectstore.GitHubImport, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return projectstore.GitHubImport{}, err
	}
	if record.ID == "" || record.ObservedAt.IsZero() {
		return projectstore.GitHubImport{}, errors.New("GitHub import identity and observation are required")
	}
	row, err := s.queries.CreateGitHubImport(ctx, pgquery.CreateGitHubImportParams{OrganizationID: s.organization, ProjectID: record.ProjectID, ID: record.ID, IssueNumber: record.IssueNumber, Stage: record.Stage, Status: record.Status, ObservedAt: record.ObservedAt})
	return importJob(row), mapError(ctx, err)
}

func (s *session) UpdateGitHubImport(ctx context.Context, record projectstore.GitHubImport) (projectstore.GitHubImport, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return projectstore.GitHubImport{}, err
	}
	if record.ObservedAt.IsZero() {
		return projectstore.GitHubImport{}, errors.New("GitHub import identity and observation are required")
	}
	gaps := record.Gaps
	if gaps == nil {
		gaps = []string{}
	}
	row, err := s.queries.UpdateGitHubImport(ctx, pgquery.UpdateGitHubImportParams{OrganizationID: s.organization, ProjectID: record.ProjectID, ID: record.ID, Revision: record.Revision, WorkItemID: record.WorkItemID, Stage: record.Stage, Cursor: record.Cursor, Pages: record.Pages, Status: record.Status, Gaps: gaps, LastError: record.LastError, RetryAfter: record.RetryAfter, SourceUpdatedAt: record.SourceUpdatedAt, ObservedAt: record.ObservedAt, IntakePending: record.IntakePending, EditSequence: record.EditSequence})
	if errors.Is(err, pgx.ErrNoRows) {
		return projectstore.GitHubImport{}, corestore.ErrRevisionConflict
	}
	return importJob(row), mapError(ctx, err)
}

func (s *session) GitHubImportForIssue(ctx context.Context, project tracker.ProjectID, number int64) (projectstore.GitHubImport, error) {
	row, err := s.queries.ReadGitHubImportForIssue(ctx, pgquery.ReadGitHubImportForIssueParams{OrganizationID: s.organization, ProjectID: project, IssueNumber: number})
	return importJob(row), mapError(ctx, err)
}
