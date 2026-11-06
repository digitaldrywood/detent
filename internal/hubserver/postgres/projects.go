package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) Project(ctx context.Context, id tracker.ProjectID) (corestore.Project, error) {
	row, err := s.queries.GetProject(ctx, pgquery.GetProjectParams{
		OrganizationID: s.organization,
		ID:             id,
	})
	return project(row), mapError(ctx, err)
}

func (s *session) CreateProject(ctx context.Context, record corestore.Project) (corestore.Project, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.Project{}, err
	}
	row, err := s.queries.CreateProject(ctx, pgquery.CreateProjectParams{
		OrganizationID:          record.OrganizationID,
		ID:                      record.ID,
		Name:                    record.Name,
		Slug:                    record.Slug,
		Profile:                 record.Profile,
		RequireDependencies:     record.RequireDependencies,
		SchedulingRank:          record.SchedulingRank,
		ContentSettingsRevision: record.ContentSettingsRevision,
		WorkflowSource:          record.WorkflowSource,
		WorkflowSourceRevision:  record.WorkflowSourceRevision,
		WorkflowObjectKey:       record.WorkflowObjectKey,
		StatesObjectKey:         record.StatesObjectKey,
		ModelSelectionObjectKey: record.ModelSelectionObjectKey,
	})
	return project(row), mapError(ctx, err)
}

func (s *session) UpdateProject(ctx context.Context, record corestore.Project) (corestore.Project, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.Project{}, err
	}
	row, err := s.queries.UpdateProject(ctx, pgquery.UpdateProjectParams{
		Name:                    record.Name,
		Slug:                    record.Slug,
		Profile:                 record.Profile,
		RequireDependencies:     record.RequireDependencies,
		SchedulingRank:          record.SchedulingRank,
		WorkflowSource:          record.WorkflowSource,
		WorkflowSourceRevision:  record.WorkflowSourceRevision,
		WorkflowObjectKey:       record.WorkflowObjectKey,
		StatesObjectKey:         record.StatesObjectKey,
		ModelSelectionObjectKey: record.ModelSelectionObjectKey,
		DeletedAt:               record.DeletedAt,
		OrganizationID:          record.OrganizationID,
		ID:                      record.ID,
		ExpectedRevision:        record.ContentSettingsRevision,
	})
	return project(row), updated(ctx, err)
}

func (s *session) DeleteProject(ctx context.Context, id tracker.ProjectID) error {
	var locked tracker.ProjectID
	if err := s.db.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, id).Scan(&locked); err != nil {
		return mapError(ctx, err)
	}
	actions, err := s.ProjectActions(ctx, id)
	if err != nil {
		return err
	}
	for _, action := range actions {
		object, err := s.ArchiveObjectByReference(ctx, action.Command)
		if err != nil {
			return err
		}
		if err := s.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(id, action.Action.ID)}); err != nil {
			return err
		}
	}
	rows, err := s.queries.DeleteProject(ctx, pgquery.DeleteProjectParams{
		OrganizationID: s.organization,
		ID:             id,
	})
	return affected(ctx, rows, err)
}

func (s *session) Projects(ctx context.Context, after tracker.ProjectID, limit int) ([]corestore.Project, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListProjects(ctx, pgquery.ListProjectsParams{
		OrganizationID: s.organization,
		After:          after,
		PageLimit:      int32(limit),
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]corestore.Project, len(rows))
	for i, row := range rows {
		records[i] = project(row)
	}
	return records, nil
}

func project(row pgquery.Project) corestore.Project {
	return corestore.Project{
		OrganizationID:          row.OrganizationID,
		ID:                      row.ID,
		Name:                    row.Name,
		Slug:                    row.Slug,
		Profile:                 row.Profile,
		RequireDependencies:     row.RequireDependencies,
		SchedulingRank:          row.SchedulingRank,
		ContentSettingsRevision: row.ContentSettingsRevision,
		WorkflowSource:          row.WorkflowSource,
		WorkflowSourceRevision:  row.WorkflowSourceRevision,
		WorkflowObjectKey:       row.WorkflowObjectKey,
		StatesObjectKey:         row.StatesObjectKey,
		ModelSelectionObjectKey: row.ModelSelectionObjectKey,
		CreatedAt:               row.CreatedAt,
		DeletedAt:               row.DeletedAt,
	}
}
