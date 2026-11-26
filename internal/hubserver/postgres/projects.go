package postgres

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/projectreads"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
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
	var name string
	if err := s.db.QueryRow(ctx, `SELECT name FROM projects WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE`, s.organization, record.ID).Scan(&name); err != nil {
		return corestore.Project{}, updated(ctx, err)
	}
	if name != record.Name {
		if err := s.lockOrganizationCounter(ctx); err != nil {
			return corestore.Project{}, err
		}
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

func (s *session) lockProjectChildren(ctx context.Context, id tracker.ProjectID, active bool) error {
	filter := ""
	if active {
		filter = " AND deleted_at IS NULL"
	}
	var locked tracker.ProjectID
	if err := s.db.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2`+filter+` FOR NO KEY UPDATE`, s.organization, id).Scan(&locked); err != nil {
		return mapError(ctx, err)
	}
	if err := s.lockOrganizationCounter(ctx); err != nil {
		return err
	}
	if err := s.db.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2`+filter+` FOR UPDATE`, s.organization, id).Scan(&locked); err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) DeleteProject(ctx context.Context, id tracker.ProjectID) error {
	if err := s.lockProjectChildren(ctx, id, false); err != nil {
		return err
	}
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return mapError(ctx, err)
	}
	active, err := s.queries.ProjectHasActiveWork(ctx, pgquery.ProjectHasActiveWorkParams{OrganizationID: s.organization, ProjectID: id, Now: now})
	if err != nil {
		return mapError(ctx, err)
	}
	if active.Bool {
		return projectstore.ErrActiveWork
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
	bytes, err := s.queries.ScheduledCIProjectStorage(ctx, pgquery.ScheduledCIProjectStorageParams{
		OrganizationID: s.organization,
		ProjectID:      id,
	})
	if err != nil {
		return fmt.Errorf("read scheduled CI decision storage: %w", mapError(ctx, err))
	}
	if bytes > 0 {
		if _, err := s.AdjustCollaborationBytes(ctx, -bytes, math.MaxInt64); err != nil {
			return fmt.Errorf("release scheduled CI decision storage: %w", err)
		}
	}
	rows, err := s.queries.DeleteProject(ctx, pgquery.DeleteProjectParams{
		OrganizationID: s.organization,
		ID:             id,
	})
	if err := affected(ctx, rows, err); err != nil {
		return err
	}
	s.boardDeltas = slices.DeleteFunc(s.boardDeltas, func(entry boardAppend) bool {
		return entry.project == id
	})
	return nil
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

func project(row pgquery.Project) corestore.Project { return projectreads.Core(row) }
