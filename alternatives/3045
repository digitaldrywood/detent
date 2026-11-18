-- +goose Up
ALTER TABLE project_event_cursors
    ADD COLUMN notification_checked_at timestamptz NOT NULL DEFAULT '1970-01-01T00:00:00Z';
DROP INDEX project_event_cursors_notification_pending_idx;
CREATE INDEX project_event_cursors_notification_pending_idx
    ON project_event_cursors (organization_id, notification_checked_at, project_id)
    WHERE notification_sequence < sequence;

-- +goose Down
DROP INDEX project_event_cursors_notification_pending_idx;
CREATE INDEX project_event_cursors_notification_pending_idx
    ON project_event_cursors (organization_id, project_id)
    WHERE notification_sequence < sequence;
ALTER TABLE project_event_cursors DROP COLUMN notification_checked_at;
