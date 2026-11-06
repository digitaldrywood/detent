-- +goose Up
CREATE INDEX usage_events_finished_at_idx
ON usage_events(finished_at, id);

CREATE INDEX usage_events_project_finished_at_idx
ON usage_events(project_id, finished_at, id);

CREATE INDEX efficiency_receipts_completed_at_idx
ON efficiency_receipts(completed_at, id);

-- +goose Down
DROP INDEX IF EXISTS efficiency_receipts_completed_at_idx;
DROP INDEX IF EXISTS usage_events_project_finished_at_idx;
DROP INDEX IF EXISTS usage_events_finished_at_idx;
