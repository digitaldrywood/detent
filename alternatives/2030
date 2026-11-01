-- +goose Up
CREATE INDEX attempt_diffs_item_source_latest_idx
ON attempt_diffs(organization_id, project_id, work_item_id, source, created_at);

-- +goose Down
DROP INDEX attempt_diffs_item_source_latest_idx;
