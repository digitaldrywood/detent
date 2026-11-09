package postgres

import "context"

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
	var err error
	if phase == 0 {
		err = s.db.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT id,project_id,native_id FROM issues WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT 1
 ), queued AS (
 INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind,propagate)
 SELECT $1,source.project_id,source.native_id,source.native_id,kinds.kind,kinds.kind='relationships'
 FROM source CROSS JOIN (VALUES ('item'),('relationships')) AS kinds(kind) ORDER BY kinds.kind ON CONFLICT DO NOTHING
 ) UPDATE native_work_item_page_progress SET backfill_phase=CASE WHEN EXISTS(SELECT FROM source) THEN 0 ELSE 1 END,
 backfill_issue_id=COALESCE((SELECT id FROM source),backfill_issue_id),examined_at=clock_timestamp()
 WHERE organization_id=$1 RETURNING backfill_phase`, s.organization, issueID).Scan(&phase)
	} else {
		err = s.db.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT id,project_id,work_item_id FROM native_comments WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT 1
 ), queued AS (
 INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind)
 SELECT $1,project_id,work_item_id,id,'comment' FROM source ON CONFLICT DO NOTHING
 ) UPDATE native_work_item_page_progress SET backfill_phase=CASE WHEN EXISTS(SELECT FROM source) THEN 1 ELSE 2 END,
 backfill_comment_id=COALESCE((SELECT id FROM source),backfill_comment_id),examined_at=clock_timestamp()
 WHERE organization_id=$1 RETURNING backfill_phase`, s.organization, commentID).Scan(&phase)
	}
	return phase == 2, mapError(ctx, err)
}
