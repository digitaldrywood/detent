package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkspaceLifecycleReader interface {
	WorkspaceReader
	WorkspaceClaimSession(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (WorkspaceClaimSession, error)
	WorkspaceSessionsByIDs(context.Context, []string) ([]WorkspaceSession, error)
	WorkspaceSubjectActors(context.Context, tracker.ProjectID, []string) (map[string]string, error)
	WorkspaceSessionByID(context.Context, string) (WorkspaceSession, error)
	WorkspaceSessionByItem(context.Context, tracker.ProjectID, string) (WorkspaceSession, error)
	WorkspaceDispatchItem(context.Context, tracker.ProjectID, string) (string, error)
	OpenWorkspaceCounts(context.Context, string) (int64, int64, error)
	OpenWorkspaceAttempt(context.Context, tracker.ProjectID, string) (WorkspaceSession, error)
	ActiveWorkspaceSessions(context.Context, string, int) ([]WorkspaceSession, error)
	RecentWorkspaceSessions(context.Context, tracker.ProjectID, string, string, int) ([]WorkspaceSession, error)
}

type WorkspaceClaimSession = pgquery.WorkspaceClaimSessionRow

func (s *session) WorkspaceClaimSession(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (WorkspaceClaimSession, error) {
	if !s.readOnly {
		row, err := s.queries.LockWorkspaceClaimSession(ctx, pgquery.LockWorkspaceClaimSessionParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
		return WorkspaceClaimSession(row), mapError(ctx, err)
	}
	row, err := s.queries.WorkspaceClaimSession(ctx, pgquery.WorkspaceClaimSessionParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
	return row, mapError(ctx, err)
}

func (s *session) WorkspaceSubjectActors(ctx context.Context, project tracker.ProjectID, ids []string) (map[string]string, error) {
	if len(ids) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.WorkspaceSubjectActors(ctx, pgquery.WorkspaceSubjectActorsParams{OrganizationID: s.organization, ProjectID: project, Column3: ids})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	actors := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ActorPrincipalID != nil {
			actors[string(row.NativeID)] = *row.ActorPrincipalID
		}
	}
	return actors, nil
}

func (s *session) WorkspaceSessionByItem(ctx context.Context, project tracker.ProjectID, item string) (WorkspaceSession, error) {
	row, err := s.queries.WorkspaceSessionByItem(ctx, pgquery.WorkspaceSessionByItemParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	return row, mapError(ctx, err)
}
func (s *session) WorkspaceDispatchItem(ctx context.Context, project tracker.ProjectID, id string) (string, error) {
	row, err := s.queries.WorkspaceDispatchItem(ctx, pgquery.WorkspaceDispatchItemParams{OrganizationID: s.organization, ProjectID: project, WorkspaceID: id})
	return string(row), mapError(ctx, err)
}
func (s *session) OpenWorkspaceCounts(ctx context.Context, actor string) (int64, int64, error) {
	row, err := s.queries.OpenWorkspaceCounts(ctx, pgquery.OpenWorkspaceCountsParams{OrganizationID: s.organization, Actor: actor})
	return row.Total, row.Mine, mapError(ctx, err)
}
func (s *session) OpenWorkspaceAttempt(ctx context.Context, project tracker.ProjectID, attempt string) (WorkspaceSession, error) {
	row, err := s.queries.OpenWorkspaceAttempt(ctx, pgquery.OpenWorkspaceAttemptParams{OrganizationID: s.organization, ProjectID: project, AttemptID: attempt})
	return row, mapError(ctx, err)
}
func (s *session) ActiveWorkspaceSessions(ctx context.Context, after string, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.ActiveWorkspaceSessions(ctx, pgquery.ActiveWorkspaceSessionsParams{OrganizationID: s.organization, ID: after, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) RecentWorkspaceSessions(ctx context.Context, project tracker.ProjectID, subject, state string, limit int) ([]WorkspaceSession, error) {
	if err := checkPage(limit); err != nil {
		return nil, corestore.ErrInvalidPage
	}
	var rows []WorkspaceSession
	var err error
	switch {
	case subject != "" && state != "":
		rows, err = s.queries.WorkspaceSubjectStateRecent(ctx, pgquery.WorkspaceSubjectStateRecentParams{OrganizationID: s.organization, ProjectID: project, SubjectWorkItemID: subject, State: state, Limit: int32(limit)})
	case subject != "":
		rows, err = s.queries.WorkspaceSubjectRecent(ctx, pgquery.WorkspaceSubjectRecentParams{OrganizationID: s.organization, ProjectID: project, SubjectWorkItemID: subject, Limit: int32(limit)})
	case state != "":
		rows, err = s.queries.WorkspaceStateRecent(ctx, pgquery.WorkspaceStateRecentParams{OrganizationID: s.organization, ProjectID: project, State: state, Limit: int32(limit)})
	default:
		rows, err = s.queries.WorkspaceProjectRecent(ctx, pgquery.WorkspaceProjectRecentParams{OrganizationID: s.organization, ProjectID: project, Limit: int32(limit)})
	}
	return rows, mapError(ctx, err)
}

func (s *session) WorkspaceSessionByID(ctx context.Context, id string) (WorkspaceSession, error) {
	row, err := s.queries.WorkspaceSessionByID(ctx, pgquery.WorkspaceSessionByIDParams{OrganizationID: s.organization, ID: id})
	return row, mapError(ctx, err)
}

func (s *session) WorkspaceSessionsByIDs(ctx context.Context, ids []string) ([]WorkspaceSession, error) {
	if len(ids) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.WorkspaceSessionsByIDs(ctx, pgquery.WorkspaceSessionsByIDsParams{OrganizationID: s.organization, Column2: ids})
	return rows, mapError(ctx, err)
}
