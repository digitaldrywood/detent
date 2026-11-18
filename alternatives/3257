-- +goose Up
CREATE INDEX collaboration_events_progress_evidence_idx
ON collaboration_events (organization_id,project_id,work_item_id,sequence DESC)
WHERE (type IN ('issue.edited','comment.created','comment.edited','run.checkpointed','run.finished') OR left(type,7)='change.')
AND NOT (type='comment.created' AND actor_kind='machine' AND actor_envelope_known AND actor_agent='progress-check');

-- +goose Down
DROP INDEX collaboration_events_progress_evidence_idx;
