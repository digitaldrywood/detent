-- +goose Up
ALTER TABLE project_event_cursors
    ADD COLUMN notification_sequence bigint NOT NULL DEFAULT 0 CHECK (notification_sequence >= 0 AND notification_sequence <= sequence);
CREATE INDEX project_event_cursors_notification_pending_idx
    ON project_event_cursors (organization_id, project_id)
    WHERE notification_sequence < sequence;

-- +goose Down
DROP INDEX project_event_cursors_notification_pending_idx;
ALTER TABLE project_event_cursors DROP COLUMN notification_sequence;
