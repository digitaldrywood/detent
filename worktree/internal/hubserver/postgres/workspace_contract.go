package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
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
	WorkspaceByDispatchItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (WorkspaceSession, error)
	WorkspaceListing(context.Context, tracker.ProjectID, string, string, int) ([]WorkspaceSession, error)
	WorkspaceOpenSessions(context.Context, time.Time, string, int) ([]WorkspaceSession, error)
	WorkspaceSession(context.Context, tracker.ProjectID, string) (WorkspaceSession, error)
	WorkspaceDispatchItem(context.Context, tracker.ProjectID, string) (claimstore.WorkspaceItem, error)
	WorkspaceSessions(context.Context, tracker.ProjectID, string, int) ([]WorkspaceSession, error)
	WorkspaceSubjectSessions(context.Context, tracker.ProjectID, string, string, int) ([]WorkspaceSession, error)
	ExpiringWorkspaces(context.Context, time.Time, int) ([]WorkspaceSession, error)
	WorkspaceOccupancy(context.Context, tracker.ProjectID, string, int) ([]WorkspaceOccupancy, error)
	IsWorkspaceWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
}

type WorkspaceWriter interface {
	WorkspaceReader
	WorkspaceOpenAttempt(context.Context, tracker.ProjectID, string) (string, error)
	WorkspaceOpenCounts(context.Context, string, int) (int, int, error)
	CreateWorkspaceSession(context.Context, WorkspaceCreate) (WorkspaceSession, error)
	UpdateWorkspaceSession(context.Context, WorkspaceUpdate) (WorkspaceSession, error)
	TouchWorkspaceActivity(context.Context, tracker.ProjectID, string, time.Time) error
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
func (s *session) WorkspaceOpenSessions(ctx context.Context, after time.Time, id string, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.WorkspaceOpenSessions(ctx, pgquery.WorkspaceOpenSessionsParams{OrganizationID: s.organization, AfterExpiresAt: after, ID: id, PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) WorkspaceByDispatchItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (WorkspaceSession, error) {
	row, err := s.queries.WorkspaceByDispatchItem(ctx, pgquery.WorkspaceByDispatchItemParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
	return row, mapError(ctx, err)
}

func (s *session) WorkspaceListing(ctx context.Context, project tracker.ProjectID, item, state string, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	var rows []WorkspaceSession
	var err error
	switch {
	case item != "" && state != "":
		rows, err = s.queries.WorkspaceSubjectStateListing(ctx, pgquery.WorkspaceSubjectStateListingParams{OrganizationID: s.organization, ProjectID: project, SubjectWorkItemID: item, State: state, PageLimit: int32(limit)})
	case item != "":
		rows, err = s.queries.WorkspaceSubjectListing(ctx, pgquery.WorkspaceSubjectListingParams{OrganizationID: s.organization, ProjectID: project, SubjectWorkItemID: item, PageLimit: int32(limit)})
	case state != "":
		rows, err = s.queries.WorkspaceStateListing(ctx, pgquery.WorkspaceStateListingParams{OrganizationID: s.organization, ProjectID: project, State: state, PageLimit: int32(limit)})
	default:
		rows, err = s.queries.WorkspaceListing(ctx, pgquery.WorkspaceListingParams{OrganizationID: s.organization, ProjectID: project, PageLimit: int32(limit)})
	}
	return rows, mapError(ctx, err)
}

func (s *session) WorkspaceSession(ctx context.Context, project tracker.ProjectID, id string) (WorkspaceSession, error) {
	row, err := s.queries.WorkspaceSession(ctx, pgquery.WorkspaceSessionParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return row, mapError(ctx, err)
}
func (s *session) WorkspaceDispatchItem(ctx context.Context, project tracker.ProjectID, id string) (claimstore.WorkspaceItem, error) {
	var item claimstore.WorkspaceItem
	err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,work_item_id,workspace_id,created_at,closed_at FROM workspace_items WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3`, s.organization, project, id).Scan(&item.OrganizationID, &item.ProjectID, &item.WorkItemID, &item.WorkspaceID, &item.CreatedAt, &item.ClosedAt)
	return item, mapError(ctx, err)
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
func (s *session) WorkspaceOpenAttempt(ctx context.Context, project tracker.ProjectID, attempt string) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT id FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 AND attempt_id<>'' AND state NOT IN ('closed','failed') LIMIT 1`, s.organization, project, attempt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, mapError(ctx, err)
}

func (s *session) WorkspaceOpenCounts(ctx context.Context, actor string, limit int) (int, int, error) {
	if limit < 1 {
		return 0, 0, corestore.ErrInvalidPage
	}
	var total, mine int
	err := s.db.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE created_by=$2) FROM (SELECT created_by FROM workspace_sessions WHERE organization_id=$1 AND state NOT IN ('closed','failed') ORDER BY expires_at,id LIMIT $3) open_sessions`, s.organization, actor, limit+1).Scan(&total, &mine)
	return total, mine, mapError(ctx, err)
}

func (s *session) CreateWorkspaceSession(ctx context.Context, arg WorkspaceCreate) (WorkspaceSession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return WorkspaceSession{}, err
	}
	requires, err := workspacesession.NormalizeRequires(arg.Requires)
	if err != nil || workspacesession.ValidateID(arg.ID) != nil || arg.CreatedBy == "" || arg.CreatedAt.IsZero() || !arg.ExpiresAt.After(arg.CreatedAt) || !arg.RequestedExpiresAt.After(arg.CreatedAt) || arg.IdleTimeoutSeconds <= 0 {
		return WorkspaceSession{}, corestore.ErrInvalidPage
	}
	arg.Requires = requires
	row, err := s.queries.CreateWorkspaceSession(ctx, arg)
	return s.workspaceMutation(row, mapError(ctx, err))
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
	retainedClose := (arg.State == workspacesession.StateClosing || workspacesession.Terminal(arg.State)) && arg.RunnerID == current.RunnerID && arg.MachineID == current.MachineID && arg.LeaseID == current.LeaseID && arg.FencingToken == current.FencingToken
	retainedRecovery := arg.State == workspacesession.StateRequested && arg.Reason == workspacesession.ReasonHubRestarted && arg.RebindDeadline != nil && arg.RebindDeadline.After(arg.UpdatedAt) && arg.RunnerID == current.RunnerID && arg.MachineID == current.MachineID && arg.LeaseID == current.LeaseID && arg.FencingToken == current.FencingToken
	if arg.LeaseID != "" && !retainedClose && !retainedRecovery {
		valid, err := s.queries.WorkspaceOwnerLease(ctx, pgquery.WorkspaceOwnerLeaseParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, WorkspaceID: arg.ID, LeaseID: arg.LeaseID, FencingToken: arg.FencingToken, MachineID: arg.MachineID, RunnerID: arg.RunnerID, LeaseClock: s.leaseClockValue()})
		if err != nil {
			return WorkspaceSession{}, mapError(ctx, err)
		}
		if !valid {
			return WorkspaceSession{}, corestore.ErrRevisionConflict
		}
	}
	row, err := s.queries.UpdateWorkspaceSession(ctx, arg)
	return s.workspaceMutation(row, updated(ctx, err))
}
func (s *session) workspaceMutation(row WorkspaceSession, err error) (WorkspaceSession, error) {
	if err != nil {
		return WorkspaceSession{}, err
	}
	revision := row.Revision
	if err := s.recordBoardDelta(row.ProjectID, viewstore.BoardDelta{WorkItemID: tracker.NativeWorkItemID(row.SubjectWorkItemID), Topic: "workspace", EntityID: row.ID, EntityRevision: &revision, Kind: "upsert", Status: row.State, RelatedID: row.SubjectWorkItemID}); err != nil {
		return WorkspaceSession{}, err
	}
	return row, nil
}
func (s *session) TouchWorkspaceActivity(ctx context.Context, project tracker.ProjectID, id string, at time.Time) error {
	if at.IsZero() {
		return corestore.ErrInvalidPage
	}
	err := s.queries.TouchWorkspaceActivity(ctx, pgquery.TouchWorkspaceActivityParams{OrganizationID: s.organization, ProjectID: project, ID: id, ActivityAt: at})
	return mapError(ctx, err)
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
