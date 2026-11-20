-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_comments_human_direction_idx
ON native_comments (organization_id, project_id, work_item_id, sequence DESC)
WHERE actor_kind = 'human' AND provenance_provider IS DISTINCT FROM 'chat';

CREATE INDEX CONCURRENTLY IF NOT EXISTS collaboration_events_human_comment_edits_idx
ON collaboration_events (organization_id, project_id, work_item_id, sequence DESC)
WHERE actor_kind = 'human' AND type = 'comment.edited';

CREATE INDEX CONCURRENTLY IF NOT EXISTS native_phase_receipts_plan_idx
ON native_phase_receipts (organization_id, project_id, issue_id, position DESC)
WHERE phase IN ('plan', 'plan_draft', 'plan_review');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_phase_receipts_plan_idx;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_human_comment_edits_idx;
DROP INDEX CONCURRENTLY IF EXISTS native_comments_human_direction_idx;
