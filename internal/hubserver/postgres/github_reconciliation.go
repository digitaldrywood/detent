package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) GitHubReconciliation(ctx context.Context, project tracker.ProjectID) (projectstore.GitHubReconciliation, error) {
	row, err := s.queries.ReadGitHubReconciliation(ctx, pgquery.ReadGitHubReconciliationParams{OrganizationID: s.organization, ProjectID: project})
	return projectstore.GitHubReconciliation{
		ProjectID: row.ProjectID, Repository: row.Repository, NodeID: row.NodeID,
		AfterWorkItem: tracker.NativeWorkItemID(row.AfterWorkItem), Revision: row.Revision, Attempts: row.Attempts, NextAttemptAt: row.NextAttemptAt,
		LastSuccessfulSweepAt: row.LastSuccessfulSweepAt, LastFailureAt: row.LastFailureAt,
	}, mapError(ctx, err)
}

func (s *session) NextGitHubReconciliation(ctx context.Context, now time.Time) (projectstore.GitHubReconciliation, error) {
	row, err := s.queries.NextGitHubReconciliation(ctx, pgquery.NextGitHubReconciliationParams{OrganizationID: s.organization, DueAt: now})
	if err != nil {
		return projectstore.GitHubReconciliation{}, mapError(ctx, err)
	}
	result := projectstore.GitHubReconciliation{ProjectID: row.ProjectID, Repository: row.Repository, Revision: row.Revision, NextAttemptAt: row.NextAttemptAt}
	if strings.EqualFold(row.StoredRepository, row.Repository) {
		result.NodeID = row.NodeID
		result.AfterWorkItem = tracker.NativeWorkItemID(row.AfterWorkItem)
		result.Attempts = row.Attempts
		result.LastSuccessfulSweepAt, result.LastFailureAt = row.LastSuccessfulSweepAt, row.LastFailureAt
	}
	return result, nil
}

func (s *session) NextGitHubLinkedSource(ctx context.Context, project tracker.ProjectID, after tracker.NativeWorkItemID) (eventstore.LinkedSource, error) {
	var id tracker.NativeWorkItemID
	err := s.db.QueryRow(ctx, `SELECT l.work_item_id FROM linked_issue_sources l JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id WHERE l.organization_id=$1 AND l.project_id=$2 AND l.work_item_id>$3 ORDER BY l.work_item_id LIMIT 1`, s.organization, project, after).Scan(&id)
	if err != nil {
		return eventstore.LinkedSource{}, mapError(ctx, err)
	}
	source, err := s.LinkedSource(ctx, project, id)
	if errors.Is(err, corestore.ErrNotFound) {
		return eventstore.LinkedSource{}, corestore.ErrRevisionConflict
	}
	return source, err
}

func (s *session) SaveGitHubReconciliation(ctx context.Context, row projectstore.GitHubReconciliation) error {
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: row.ProjectID}); err != nil {
		return mapError(ctx, err)
	}
	repository := strings.ToLower(row.Repository)
	if row.Revision == 0 {
		count, err := s.queries.InitializeGitHubReconciliation(ctx, pgquery.InitializeGitHubReconciliationParams{
			OrganizationID: s.organization, ProjectID: row.ProjectID, Repository: repository, NodeID: row.NodeID,
			AfterWorkItem: string(row.AfterWorkItem), Attempts: row.Attempts, NextAttemptAt: row.NextAttemptAt,
			LastSuccessfulSweepAt: row.LastSuccessfulSweepAt, LastFailureAt: row.LastFailureAt,
		})
		if err != nil {
			return mapError(ctx, err)
		}
		if count != 1 {
			return corestore.ErrRevisionConflict
		}
		return mapError(ctx, s.queries.MarkGitHubReconciliationInitialized(ctx, pgquery.MarkGitHubReconciliationInitializedParams{OrganizationID: s.organization, ProjectID: row.ProjectID}))
	}
	count, err := s.queries.AdvanceGitHubReconciliation(ctx, pgquery.AdvanceGitHubReconciliationParams{
		OrganizationID: s.organization, ProjectID: row.ProjectID, Repository: repository, NodeID: row.NodeID,
		AfterWorkItem: string(row.AfterWorkItem), ExpectedRevision: row.Revision, Attempts: row.Attempts, NextAttemptAt: row.NextAttemptAt,
		LastSuccessfulSweepAt: row.LastSuccessfulSweepAt, LastFailureAt: row.LastFailureAt,
	})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
