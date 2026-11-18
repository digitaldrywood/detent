package postgres

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) cursor(project tracker.ProjectID, item tracker.NativeWorkItemID, created time.Time, id int64) corestore.Cursor {
	return corestore.Cursor{Version: 2, OrganizationID: s.organization, ProjectID: project, WorkItemID: item, CreatedAt: created, ID: id}
}

func (s *session) checkCursor(project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.Cursor) error {
	if cursor == (corestore.Cursor{}) {
		return nil
	}
	if cursor.OrganizationID != s.organization || cursor.ProjectID != project || cursor.WorkItemID != item || cursor.ID == 0 {
		return corestore.ErrInvalidCursor
	}
	if cursor.Version == 1 && cursor.CreatedAt.IsZero() || cursor.Version == 2 && !cursor.CreatedAt.IsZero() {
		return nil
	}
	return corestore.ErrInvalidCursor
}

func (s *session) WorkItemPage(ctx context.Context, project tracker.ProjectID, cursor corestore.Cursor, limit int) (corestore.Page[corestore.WorkItem], error) {
	var page corestore.Page[corestore.WorkItem]
	if err := checkPage(limit); err != nil {
		return page, err
	}
	if err := s.checkCursor(project, "", cursor); err != nil {
		return page, err
	}
	if cursor.Version == 1 {
		_, err := s.queries.GetWorkItemCursor(ctx, pgquery.GetWorkItemCursorParams{OrganizationID: s.organization, ProjectID: project, ID: cursor.ID})
		if err != nil {
			return page, cursorError(ctx, err)
		}
		scan, err := s.ScanWorkItems(ctx, project, corestore.ScanCursor{After: cursor.ID, Through: math.MaxInt64}, limit)
		if err != nil {
			return page, err
		}
		page.Items = scan.Items
		if len(page.Items) == limit {
			cursor.ID = page.Items[len(page.Items)-1].ID
			page.Next = cursor
		}
		return page, nil
	}
	rows, err := s.queries.PageWorkItems(ctx, pgquery.PageWorkItemsParams{OrganizationID: s.organization, ProjectID: project, AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID, PageLimit: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Items = make([]corestore.WorkItem, len(rows))
	for i, row := range rows {
		page.Items[i] = corestore.WorkItem(row)
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		page.Next = s.cursor(project, "", last.CreatedAt, last.ID)
	}
	return page, nil
}

func (s *session) CommentPage(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.Cursor, limit int) (corestore.Page[corestore.Comment], error) {
	var page corestore.Page[corestore.Comment]
	if err := checkPage(limit); err != nil {
		return page, err
	}
	if err := s.checkCursor(project, item, cursor); err != nil {
		return page, err
	}
	if cursor.Version == 1 {
		_, err := s.queries.GetCommentCursor(ctx, pgquery.GetCommentCursorParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, ID: cursor.ID})
		if err != nil {
			return page, cursorError(ctx, err)
		}
		scan, err := s.ScanComments(ctx, project, item, corestore.ScanCursor{After: cursor.ID, Through: math.MaxInt64}, limit)
		if err != nil {
			return page, err
		}
		page.Items = scan.Items
		if len(page.Items) == limit {
			cursor.ID = page.Items[len(page.Items)-1].RecordID
			page.Next = cursor
		}
		return page, nil
	}
	rows, err := s.queries.PageComments(ctx, pgquery.PageCommentsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID, PageLimit: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Items = make([]corestore.Comment, len(rows))
	for i, row := range rows {
		page.Items[i] = corestore.Comment(row)
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		page.Next = s.cursor(project, item, last.CreatedAt, last.RecordID)
	}
	return page, nil
}

func cursorError(ctx context.Context, err error) error {
	err = mapError(ctx, err)
	if errors.Is(err, corestore.ErrNotFound) {
		return corestore.ErrInvalidCursor
	}
	return err
}

func (s *session) ScanWorkItems(ctx context.Context, project tracker.ProjectID, cursor corestore.ScanCursor, limit int) (corestore.Scan[corestore.WorkItem], error) {
	var page corestore.Scan[corestore.WorkItem]
	if err := checkPage(limit); err != nil {
		return page, err
	}
	if cursor == (corestore.ScanCursor{}) {
		through, err := s.queries.WorkItemScanThrough(ctx, pgquery.WorkItemScanThroughParams{OrganizationID: s.organization, ProjectID: project})
		if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
			return page, nil
		}
		if err != nil {
			return page, mapError(ctx, err)
		}
		cursor = corestore.ScanCursor{After: math.MinInt64, Through: through}
	}
	rows, err := s.queries.ScanWorkItems(ctx, pgquery.ScanWorkItemsParams{OrganizationID: s.organization, ProjectID: project, AfterID: cursor.After, ThroughID: cursor.Through, PageLimit: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Items = make([]corestore.WorkItem, len(rows))
	for i, row := range rows {
		page.Items[i] = corestore.WorkItem(row)
	}
	if len(rows) == limit && rows[len(rows)-1].ID < cursor.Through {
		page.Next = corestore.ScanCursor{After: rows[len(rows)-1].ID, Through: cursor.Through}
	}
	return page, nil
}

func (s *session) ScanComments(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.ScanCursor, limit int) (corestore.Scan[corestore.Comment], error) {
	var page corestore.Scan[corestore.Comment]
	if err := checkPage(limit); err != nil {
		return page, err
	}
	if cursor == (corestore.ScanCursor{}) {
		through, err := s.queries.CommentScanThrough(ctx, pgquery.CommentScanThroughParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
		if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
			return page, nil
		}
		if err != nil {
			return page, mapError(ctx, err)
		}
		cursor = corestore.ScanCursor{After: math.MinInt64, Through: through}
	}
	rows, err := s.queries.ScanComments(ctx, pgquery.ScanCommentsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, AfterID: cursor.After, ThroughID: cursor.Through, PageLimit: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Items = make([]corestore.Comment, len(rows))
	for i, row := range rows {
		page.Items[i] = corestore.Comment(row)
	}
	if len(rows) == limit && rows[len(rows)-1].RecordID < cursor.Through {
		page.Next = corestore.ScanCursor{After: rows[len(rows)-1].RecordID, Through: cursor.Through}
	}
	return page, nil
}
