package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type WorkspaceSession = pgquery.WorkspaceSession
type WorkspaceCreate = pgquery.CreateWorkspaceSessionParams
type WorkspaceUpdate = pgquery.UpdateWorkspaceSessionParams
type WorkspaceOccupancy = pgquery.WorkspaceOccupancy
type WorkspaceOccupancyStart = pgquery.CreateWorkspaceOccupancyParams
type WorkspaceOccupancyEnd = pgquery.EndWorkspaceOccupancyParams

type WorkspaceReader interface {
	WorkspaceSession(context.Context, tracker.ProjectID, string) (WorkspaceSession, error)
	WorkspaceSessions(context.Context, tracker.ProjectID, string, int) ([]WorkspaceSession, error)
	WorkspaceSubjectSessions(context.Context, tracker.ProjectID, string, string, int) ([]WorkspaceSession, error)
	ExpiringWorkspaces(context.Context, time.Time, int) ([]WorkspaceSession, error)
	WorkspaceOccupancy(context.Context, tracker.ProjectID, string, int) ([]WorkspaceOccupancy, error)
	IsWorkspaceWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
}

type WorkspaceWriter interface {
	WorkspaceReader
	CreateWorkspaceSession(context.Context, WorkspaceCreate) (WorkspaceSession, error)
	UpdateWorkspaceSession(context.Context, WorkspaceUpdate) (WorkspaceSession, error)
	CreateWorkspaceOccupancy(context.Context, WorkspaceOccupancyStart) error
	EndWorkspaceOccupancy(context.Context, WorkspaceOccupancyEnd) error
	PutWorkspaceItem(context.Context, claimstore.WorkspaceItem) error
	CloseWorkspaceItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, time.Time) error
}

func (r *Repository) ReadWorkspace(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(WorkspaceReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return read(s) })
}
func (r *Repository) WriteWorkspace(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(WorkspaceWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return write(s) })
}
func (s *session) WorkspaceSession(ctx context.Context, project tracker.ProjectID, id string) (WorkspaceSession, error) {
	row, err := s.queries.WorkspaceSession(ctx, pgquery.WorkspaceSessionParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return row, mapError(ctx, err)
}
func (s *session) WorkspaceSessions(ctx context.Context, project tracker.ProjectID, after string, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.WorkspaceSessions(ctx, pgquery.WorkspaceSessionsParams{OrganizationID: s.organization, ProjectID: project, ID: after, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) WorkspaceSubjectSessions(ctx context.Context, project tracker.ProjectID, item, after string, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.WorkspaceSubjectSessions(ctx, pgquery.WorkspaceSubjectSessionsParams{OrganizationID: s.organization, ProjectID: project, SubjectWorkItemID: item, ID: after, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) CreateWorkspaceSession(ctx context.Context, arg WorkspaceCreate) (WorkspaceSession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return WorkspaceSession{}, err
	}
	requires, err := workspacesession.NormalizeRequires(arg.Requires)
	if err != nil || workspacesession.ValidateID(arg.ID) != nil || arg.Ref == "" || arg.CreatedBy == "" || arg.CreatedAt.IsZero() || !arg.ExpiresAt.After(arg.CreatedAt) || !arg.RequestedExpiresAt.After(arg.CreatedAt) || arg.IdleTimeoutSeconds <= 0 {
		return WorkspaceSession{}, corestore.ErrInvalidPage
	}
	arg.Requires = requires
	row, err := s.queries.CreateWorkspaceSession(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) UpdateWorkspaceSession(ctx context.Context, arg WorkspaceUpdate) (WorkspaceSession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return WorkspaceSession{}, err
	}
	current, err := s.WorkspaceSession(ctx, arg.ProjectID, arg.ID)
	if err != nil {
		return WorkspaceSession{}, err
	}
	if current.Revision != arg.ExpectedRevision || current.RunnerID != arg.ExpectedRunnerID || current.LeaseID != arg.ExpectedLeaseID || current.FencingToken != arg.ExpectedFencingToken {
		return WorkspaceSession{}, corestore.ErrRevisionConflict
	}
	if arg.UpdatedAt.IsZero() || arg.UpdatedAt.Before(current.UpdatedAt) || !workspacesession.ValidState(arg.State) || current.State != arg.State && !workspacesession.Transition(current.State, arg.State) {
		return WorkspaceSession{}, corestore.ErrInvalidPage
	}
	for _, name := range arg.Capabilities {
		if !workspacesession.ValidCapability(name) {
			return WorkspaceSession{}, corestore.ErrInvalidPage
		}
	}
	if arg.LeaseID != "" {
		valid, err := s.queries.WorkspaceOwnerLease(ctx, pgquery.WorkspaceOwnerLeaseParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, WorkspaceID: arg.ID, LeaseID: arg.LeaseID, FencingToken: arg.FencingToken, MachineID: arg.MachineID, RunnerID: arg.RunnerID})
		if err != nil {
			return WorkspaceSession{}, mapError(ctx, err)
		}
		if !valid {
			return WorkspaceSession{}, corestore.ErrRevisionConflict
		}
	}
	row, err := s.queries.UpdateWorkspaceSession(ctx, arg)
	return row, updated(ctx, err)
}
func (s *session) ExpiringWorkspaces(ctx context.Context, now time.Time, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ExpiringWorkspaces(ctx, pgquery.ExpiringWorkspacesParams{OrganizationID: s.organization, ExpiresAt: now, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) WorkspaceOccupancy(ctx context.Context, project tracker.ProjectID, id string, limit int) ([]WorkspaceOccupancy, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.WorkspaceOccupancy(ctx, pgquery.WorkspaceOccupancyParams{OrganizationID: s.organization, ProjectID: project, WorkspaceID: id, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) CreateWorkspaceOccupancy(ctx context.Context, arg WorkspaceOccupancyStart) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.StartedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	_, err := s.queries.CreateWorkspaceOccupancy(ctx, arg)
	return mapError(ctx, err)
}
func (s *session) EndWorkspaceOccupancy(ctx context.Context, arg WorkspaceOccupancyEnd) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.EndedAt == nil || arg.EndedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	_, err := s.queries.EndWorkspaceOccupancy(ctx, arg)
	return mapError(ctx, err)
}
