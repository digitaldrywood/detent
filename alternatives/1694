-- +goose Up
ALTER TABLE issue_notification_cursors ADD COLUMN recipient_item TEXT NOT NULL DEFAULT '';
ALTER TABLE issue_notification_cursors ADD COLUMN recipient_principal TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE issue_notification_cursors DROP COLUMN recipient_principal;
ALTER TABLE issue_notification_cursors DROP COLUMN recipient_item;
