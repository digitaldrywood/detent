-- +goose Up
CREATE INDEX collaboration_events_scheduler_outcome_idx
ON collaboration_events(organization_id, project_id, work_item_id, json_extract(data_json, '$.decision.outcome'), sequence DESC)
WHERE type = 'scheduler.decision';

-- +goose Down
DROP INDEX collaboration_events_scheduler_outcome_idx;
