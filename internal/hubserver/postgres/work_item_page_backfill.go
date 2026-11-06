package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) BackfillWorkItemPages(ctx context.Context) (bool, error) {
	if _, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_progress(organization_id) VALUES($1) ON CONFLICT DO NOTHING`, s.organization); err != nil {
		return false, mapError(ctx, err)
	}
	var phase int
	var issueID int64
	var commentID string
	if err := s.db.QueryRow(ctx, `SELECT backfill_phase,backfill_issue_id,backfill_comment_id FROM native_work_item_page_progress WHERE organization_id=$1 FOR UPDATE`, s.organization).Scan(&phase, &issueID, &commentID); err != nil {
		return false, mapError(ctx, err)
	}
	if phase == 2 {
		return true, nil
	}
	var project tracker.ProjectID
	var item tracker.NativeWorkItemID
	var err error
	if phase == 0 {
		err = s.db.QueryRow(ctx, `SELECT id,project_id,native_id FROM issues WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT 1`, s.organization, issueID).Scan(&issueID, &project, &item)
	} else {
		err = s.db.QueryRow(ctx, `SELECT id,project_id,work_item_id FROM native_comments WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT 1`, s.organization, commentID).Scan(&commentID, &project, &item)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		phase++
	} else if err != nil {
		return false, mapError(ctx, err)
	} else {
		kind, record := "item", string(item)
		if phase == 1 {
			kind, record = "comment", commentID
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, s.organization, project, item, record, kind); err != nil {
			return false, mapError(ctx, err)
		}
		if phase == 0 {
			if _, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind,propagate) VALUES($1,$2,$3,$3,'relationships',true) ON CONFLICT DO NOTHING`, s.organization, project, item); err != nil {
				return false, mapError(ctx, err)
			}
		}
	}
	_, err = s.db.Exec(ctx, `UPDATE native_work_item_page_progress SET backfill_phase=$2,backfill_issue_id=$3,backfill_comment_id=$4,examined_at=clock_timestamp() WHERE organization_id=$1`, s.organization, phase, issueID, commentID)
	return phase == 2, mapError(ctx, err)
}
