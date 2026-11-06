package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) putBoardPage(ctx context.Context, b viewstore.BoardRecord) error {
	if err := s.putBoardModel(ctx, b, b.ListIssue, boardListColumns); err != nil {
		return err
	}
	return s.putBoardModel(ctx, b, b.PageIssue, boardPageColumns)
}

func (s *session) putBoardModel(ctx context.Context, b viewstore.BoardRecord, issue *tracker.NativeIssue, columns string) error {
	if issue != nil && (issue.OrganizationID != b.OrganizationID || issue.ProjectID != b.ProjectID || issue.WorkItemID != b.WorkItemID || int64(issue.Revision) != b.SourceRevision) {
		return viewstore.ErrInvalidView
	}
	m := newBoardPageModel(issue, b.ReporterRelations)
	m.projectedAt = b.ProjectedAt
	args := append([]any{s.organization, b.ProjectID, b.WorkItemID}, m.values()...)
	names := strings.Split(strings.ReplaceAll(columns, "b.", ""), ",")
	for i, name := range names {
		names[i] = fmt.Sprintf("%s=$%d", strings.TrimSpace(name), i+4)
	}
	_, err := s.db.Exec(ctx, "UPDATE native_board_items SET "+strings.Join(names, ",")+" WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", args...)
	return mapError(ctx, err)
}
