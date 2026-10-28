-- +goose Up
ALTER TABLE projects ADD COLUMN require_visual_attachments INTEGER NOT NULL DEFAULT 0 CHECK (require_visual_attachments IN (0, 1));
ALTER TABLE projects ADD COLUMN content_settings_revision INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE projects DROP COLUMN content_settings_revision;
ALTER TABLE projects DROP COLUMN require_visual_attachments;
