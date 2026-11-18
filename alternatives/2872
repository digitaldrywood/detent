package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
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
		LastSuccessfulSweepAt: row.LastSuccessfulSweepAt, LastFailureAt: row.LastFailureAt, RepositoryPage: row.RepositoryPage, SweepStartedAt: row.SweepStartedAt,
	}, mapError(ctx, err)
}

func (s *session) DueGitHubReconciliations(ctx context.Context, now time.Time, limit int, linkedIntake bool) ([]projectstore.GitHubReconciliation, error) {
	if limit < 1 || limit > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.DueGitHubReconciliations(ctx, pgquery.DueGitHubReconciliationsParams{OrganizationID: s.organization, DueAt: now, RowLimit: int32(limit), LinkedIntake: linkedIntake})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]projectstore.GitHubReconciliation, 0, len(rows))
	for _, row := range rows {
		item := projectstore.GitHubReconciliation{ProjectID: row.ProjectID, Repository: row.Repository, RepositoryEnabled: row.RepositoryEnabled, Revision: row.Revision, NextAttemptAt: row.NextAttemptAt}
		if strings.EqualFold(row.StoredRepository, row.Repository) {
			item.NodeID = row.NodeID
			item.AfterWorkItem = tracker.NativeWorkItemID(row.AfterWorkItem)
			item.Attempts = row.Attempts
			item.LastSuccessfulSweepAt, item.LastFailureAt = row.LastSuccessfulSweepAt, row.LastFailureAt
			item.RepositoryPage, item.SweepStartedAt = row.RepositoryPage, row.SweepStartedAt
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *session) RequestDriftedGitHubPullRequests(ctx context.Context, project tracker.ProjectID, repository string, drift []projectstore.GitHubPullRequestDrift, now time.Time) (int64, error) {
	if project == "" || repository == "" || repository != strings.ToLower(repository) || now.IsZero() {
		return 0, artifact.ErrIntegrity
	}
	if len(drift) == 0 {
		return 0, nil
	}
	if err := checkPage(len(drift)); err != nil {
		return 0, err
	}
	numbers, updated := make([]int64, 0, len(drift)), make([]time.Time, 0, len(drift))
	for _, row := range drift {
		if row.Number <= 0 || row.SourceUpdatedAt.IsZero() {
			return 0, artifact.ErrIntegrity
		}
		numbers, updated = append(numbers, int64(row.Number)), append(updated, row.SourceUpdatedAt)
	}
	tag, err := s.db.Exec(ctx, `UPDATE github_pull_request_refresh r SET
 request_count=r.request_count+1,
 requested_at=GREATEST(r.requested_at,$5::timestamptz),
 next_attempt_at=$5::timestamptz,
 pending=true,
 attempts=0
FROM UNNEST($3::bigint[],$4::timestamptz[]) AS drift(pull_number,source_updated_at)
WHERE r.organization_id=$1 AND r.project_id=$2 AND r.repository=$6
  AND r.source_kind='change' AND NOT r.pending AND r.pull_number=drift.pull_number
  AND r.observed_at IS NOT NULL AND r.source_updated_at<drift.source_updated_at
  AND EXISTS(
   SELECT 1 FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
   WHERE p.organization_id=r.organization_id AND p.id=r.project_id AND p.deleted_at IS NULL AND p.profile='native'
     AND ps.repository_enabled AND lower(ps.checkout_repository)=r.repository)`, s.organization, project, numbers, updated, now, repository)
	if err != nil {
		return 0, mapError(ctx, err)
	}
	return tag.RowsAffected(), nil
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
			LastSuccessfulSweepAt: row.LastSuccessfulSweepAt, LastFailureAt: row.LastFailureAt, RepositoryPage: row.RepositoryPage, SweepStartedAt: row.SweepStartedAt,
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
		LastSuccessfulSweepAt: row.LastSuccessfulSweepAt, LastFailureAt: row.LastFailureAt, RepositoryPage: row.RepositoryPage, SweepStartedAt: row.SweepStartedAt,
	})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
