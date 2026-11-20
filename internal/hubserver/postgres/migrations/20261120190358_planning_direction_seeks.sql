-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS native_comments_human_direction_idx;
CREATE INDEX CONCURRENTLY native_comments_human_direction_idx
ON native_comments (organization_id, project_id, work_item_id, sequence DESC)
WHERE actor_kind = 'human' AND provenance_provider IS DISTINCT FROM 'chat';

DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_human_comment_edits_idx;
CREATE INDEX CONCURRENTLY collaboration_events_human_comment_edits_idx
ON collaboration_events (organization_id, project_id, work_item_id, sequence DESC)
WHERE actor_kind = 'human' AND type = 'comment.edited';

DROP INDEX CONCURRENTLY IF EXISTS native_phase_receipts_plan_idx;
CREATE INDEX CONCURRENTLY native_phase_receipts_plan_idx
ON native_phase_receipts (organization_id, project_id, issue_id, position DESC)
WHERE phase IN ('plan', 'plan_draft', 'plan_review');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_phase_receipts_plan_idx;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_human_comment_edits_idx;
DROP INDEX CONCURRENTLY IF EXISTS native_comments_human_direction_idx;
