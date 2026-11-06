package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) OrganizationSchedulingRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRow(ctx, `SELECT scheduling_revision FROM organizations WHERE organization_id=$1`, s.organization).Scan(&revision)
	return revision, mapError(ctx, err)
}
func (s *session) OrganizationRankProjects(ctx context.Context, limit int) ([]corestore.Project, error) {
	if limit < 1 || limit > 201 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT id,scheduling_rank,created_at FROM projects WHERE organization_id=$1 AND deleted_at IS NULL ORDER BY scheduling_rank,created_at,id LIMIT $2`, s.organization, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []corestore.Project{}
	for rows.Next() {
		var row corestore.Project
		if err := rows.Scan(&row.ID, &row.SchedulingRank, &row.CreatedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) SetOrganizationProjectRanks(ctx context.Context, expected int64, ids []tracker.ProjectID) error {
	if len(ids) > 200 {
		return corestore.ErrInvalidPage
	}
	if err := s.LockAccessOrganization(ctx); err != nil {
		return err
	}
	revision, err := s.OrganizationSchedulingRevision(ctx)
	if err != nil {
		return err
	}
	if revision != expected {
		return corestore.ErrRevisionConflict
	}
	for rank, id := range ids {
		result, err := s.db.Exec(ctx, `UPDATE projects SET scheduling_rank=$3 WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL`, s.organization, id, rank)
		if err != nil {
			return mapError(ctx, err)
		}
		n := result.RowsAffected()
		if err := affected(ctx, n, err); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(ctx, `UPDATE organizations SET scheduling_revision=scheduling_revision+1 WHERE organization_id=$1`, s.organization)
	return mapError(ctx, err)
}

func (s *session) AdoptOrganizationSchedulingRevision(ctx context.Context, row projectstore.OrganizationSchedulingAdoption) error {
	if row.OrganizationID != s.organization || row.ExpectedRevision != 1 || row.SourceRevision < 1 || row.ManifestSHA256 == [32]byte{} || row.SourceSHA256 == [32]byte{} || len(row.Projects) > 200 {
		return corestore.ErrInvalidPage
	}
	expected := make(map[tracker.ProjectID]int64, len(row.Projects))
	for _, project := range row.Projects {
		if project.ProjectID == "" {
			return corestore.ErrInvalidPage
		}
		if _, exists := expected[project.ProjectID]; exists {
			return corestore.ErrInvalidPage
		}
		expected[project.ProjectID] = project.Rank
	}
	if err := s.LockAccessOrganization(ctx); err != nil {
		return err
	}
	revision, err := s.OrganizationSchedulingRevision(ctx)
	if err != nil {
		return err
	}
	projects, err := s.OrganizationRankProjects(ctx, 201)
	if err != nil {
		return err
	}
	if len(projects) != len(expected) {
		return corestore.ErrRevisionConflict
	}
	for _, project := range projects {
		rank, exists := expected[project.ID]
		if !exists || rank != project.SchedulingRank {
			return corestore.ErrRevisionConflict
		}
	}
	if revision == row.SourceRevision {
		return nil
	}
	if revision != row.ExpectedRevision {
		return corestore.ErrRevisionConflict
	}
	result, err := s.db.Exec(ctx, `UPDATE organizations SET scheduling_revision=$3 WHERE organization_id=$1 AND scheduling_revision=$2`, s.organization, row.ExpectedRevision, row.SourceRevision)
	if err != nil {
		return mapError(ctx, err)
	}
	count := result.RowsAffected()
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
