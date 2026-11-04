package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) NextWorkItemNumber(ctx context.Context, project tracker.ProjectID) (int64, error) {
	value, err := s.queries.NextWorkItemNumber(ctx, pgquery.NextWorkItemNumberParams{OrganizationID: s.organization, ProjectID: project})
	return value, mapError(ctx, err)
}

func (s *session) LockWorkItemProject(ctx context.Context, project tracker.ProjectID) error {
	_, err := s.queries.LockWorkItemProject(ctx, pgquery.LockWorkItemProjectParams{OrganizationID: s.organization, ProjectID: project})
	return mapError(ctx, err)
}

func (s *session) LockWorkItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	if err := s.LockWorkItemProject(ctx, project); err != nil {
		return corestore.WorkItem{}, err
	}
	row, err := s.queries.LockWorkItem(ctx, pgquery.LockWorkItemParams{OrganizationID: s.organization, ProjectID: project, NativeID: item})
	return workItemRecord(row), mapError(ctx, err)
}

func (s *session) WorkItemBySource(ctx context.Context, project tracker.ProjectID, source string) (corestore.WorkItem, error) {
	row, err := s.queries.WorkItemBySource(ctx, pgquery.WorkItemBySourceParams{OrganizationID: s.organization, ProjectID: project, SourceKey: &source})
	return workItemRecord(row), mapError(ctx, err)
}

func (s *session) CommentBySource(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, source string) (corestore.Comment, error) {
	row, err := s.queries.CommentBySource(ctx, pgquery.CommentBySourceParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, SourceKey: &source})
	return corestore.Comment(row), mapError(ctx, err)
}

func (s *session) SelectComments(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, newest bool, limit int) ([]corestore.Comment, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	var rows []pgquery.NativeComment
	var err error
	if newest {
		rows, err = s.queries.SelectCommentsNewest(ctx, pgquery.SelectCommentsNewestParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, AfterSequence: after, PageLimit: int32(limit)})
	} else {
		rows, err = s.queries.SelectCommentsOldest(ctx, pgquery.SelectCommentsOldestParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, AfterSequence: after, PageLimit: int32(limit)})
	}
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]corestore.Comment, len(rows))
	for i, row := range rows {
		records[i] = corestore.Comment(row)
	}
	return records, nil
}

func (s *session) WorkItemHasActiveLease(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, now time.Time) (bool, error) {
	value, err := s.queries.WorkItemHasActiveLease(ctx, pgquery.WorkItemHasActiveLeaseParams{OrganizationID: s.organization, ProjectID: project, NativeID: item, Now: now})
	return value, mapError(ctx, err)
}
