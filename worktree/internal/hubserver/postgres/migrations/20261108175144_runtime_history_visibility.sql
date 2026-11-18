-- +goose Up
-- +goose StatementBegin
ALTER TABLE collaboration_events ADD COLUMN history_hidden boolean NOT NULL DEFAULT false;
CREATE INDEX collaboration_events_visible_history ON collaboration_events (organization_id, project_id, work_item_id, sequence) WHERE NOT history_hidden;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX collaboration_events_visible_history;
ALTER TABLE collaboration_events DROP COLUMN history_hidden;
-- +goose StatementEnd
