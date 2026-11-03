-- +goose Up
CREATE INDEX collaboration_events_native_intent ON collaboration_events(work_item_id, sequence DESC) WHERE type != 'github.imported';

-- +goose Down
DROP INDEX collaboration_events_native_intent;
