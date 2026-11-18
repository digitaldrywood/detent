package postgres

import "context"

const fingerprintBackfillBatch = 200

const stampedPageBody = "(position('```detent-origin' IN p.body)>0 OR position('detent-audit-fp' IN p.body)>0)"

func (s *session) BackfillWorkItemPages(ctx context.Context) (bool, error) {
	if _, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_progress(organization_id) VALUES($1) ON CONFLICT DO NOTHING`, s.organization); err != nil {
		return false, mapError(ctx, err)
	}
	var phase, fingerprintPhase int
	var issueID, fingerprintIssueID int64
	var commentID, fingerprintCommentID string
	if err := s.db.QueryRow(ctx, `SELECT backfill_phase,backfill_issue_id,backfill_comment_id,fingerprint_phase,fingerprint_issue_id,fingerprint_comment_id FROM native_work_item_page_progress WHERE organization_id=$1`, s.organization).Scan(&phase, &issueID, &commentID, &fingerprintPhase, &fingerprintIssueID, &fingerprintCommentID); err != nil {
		return false, mapError(ctx, err)
	}
	switch {
	case phase == 0:
		var next *int64
		if err := s.db.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT id,project_id,native_id FROM issues WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT 1
 ), queued AS (
 INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind,propagate)
 SELECT $1,source.project_id,source.native_id,source.native_id,kinds.kind,kinds.kind='relationships'
 FROM source CROSS JOIN (VALUES ('item'),('relationships')) AS kinds(kind) ORDER BY kinds.kind ON CONFLICT DO NOTHING
 ) SELECT (SELECT id FROM source)`, s.organization, issueID).Scan(&next); err != nil {
			return false, mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `UPDATE native_work_item_page_progress SET backfill_phase=CASE WHEN $3::bigint IS NULL THEN 1 ELSE 0 END,
 backfill_issue_id=COALESCE($3,backfill_issue_id),examined_at=clock_timestamp()
 WHERE organization_id=$1 AND backfill_phase=0 AND backfill_issue_id=$2`, s.organization, issueID, next)
		return false, mapError(ctx, err)
	case phase == 1:
		var next *string
		if err := s.db.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT id,project_id,work_item_id FROM native_comments WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT 1
 ), queued AS (
 INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind)
 SELECT $1,project_id,work_item_id,id,'comment' FROM source ON CONFLICT DO NOTHING
 ) SELECT (SELECT id FROM source)`, s.organization, commentID).Scan(&next); err != nil {
			return false, mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `UPDATE native_work_item_page_progress SET backfill_phase=CASE WHEN $3::text IS NULL THEN 2 ELSE 1 END,
 backfill_comment_id=COALESCE($3,backfill_comment_id),examined_at=clock_timestamp()
 WHERE organization_id=$1 AND backfill_phase=1 AND backfill_comment_id=$2`, s.organization, commentID, next)
		return false, mapError(ctx, err)
	case fingerprintPhase == 0:
		var last *int64
		var examined int
		if err := s.db.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT i.id,i.project_id,i.native_id FROM issues i WHERE i.organization_id=$1 AND i.id>$2 ORDER BY i.id LIMIT $3
 ), queued AS (
 INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind)
 SELECT $1,source.project_id,source.native_id,source.native_id,'item' FROM source
 JOIN native_work_item_pages p ON p.organization_id=$1 AND p.project_id=source.project_id AND p.work_item_id=source.native_id
 WHERE `+stampedPageBody+` ON CONFLICT DO NOTHING
 ) SELECT (SELECT max(id) FROM source),(SELECT count(*) FROM source)`, s.organization, fingerprintIssueID, fingerprintBackfillBatch).Scan(&last, &examined); err != nil {
			return false, mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `UPDATE native_work_item_page_progress SET fingerprint_phase=CASE WHEN $4::integer<$5::integer THEN 1 ELSE 0 END,
 fingerprint_issue_id=COALESCE($3,fingerprint_issue_id),examined_at=clock_timestamp()
 WHERE organization_id=$1 AND backfill_phase=2 AND fingerprint_phase=0 AND fingerprint_issue_id=$2`, s.organization, fingerprintIssueID, last, examined, fingerprintBackfillBatch)
		return false, mapError(ctx, err)
	case fingerprintPhase == 1:
		var last *string
		var examined int
		if err := s.db.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT c.id,c.project_id,c.work_item_id FROM native_comments c WHERE c.organization_id=$1 AND c.id>$2 ORDER BY c.id LIMIT $3
 ), queued AS (
 INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind)
 SELECT $1,source.project_id,source.work_item_id,source.id,'comment' FROM source
 JOIN native_comment_pages p ON p.organization_id=$1 AND p.project_id=source.project_id AND p.work_item_id=source.work_item_id AND p.comment_id=source.id
 WHERE `+stampedPageBody+` ON CONFLICT DO NOTHING
 ) SELECT (SELECT max(id) FROM source),(SELECT count(*) FROM source)`, s.organization, fingerprintCommentID, fingerprintBackfillBatch).Scan(&last, &examined); err != nil {
			return false, mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `UPDATE native_work_item_page_progress SET fingerprint_phase=CASE WHEN $4::integer<$5::integer THEN 2 ELSE 1 END,
 fingerprint_comment_id=COALESCE($3,fingerprint_comment_id),examined_at=clock_timestamp()
 WHERE organization_id=$1 AND backfill_phase=2 AND fingerprint_phase=1 AND fingerprint_comment_id=$2`, s.organization, fingerprintCommentID, last, examined, fingerprintBackfillBatch)
		return false, mapError(ctx, err)
	}
	return true, nil
}
