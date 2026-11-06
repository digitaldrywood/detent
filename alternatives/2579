-- +goose Up
CREATE INDEX collaboration_events_attempt_latest_idx
ON collaboration_events (organization_id, project_id, work_item_id, attempt_id, sequence DESC)
WHERE attempt_id <> '';

-- +goose Down
DROP INDEX collaboration_events_attempt_latest_idx;
