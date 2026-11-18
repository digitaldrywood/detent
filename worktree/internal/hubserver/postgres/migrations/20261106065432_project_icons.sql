-- +goose Up
ALTER TABLE project_settings
 ADD COLUMN icon_kind text NOT NULL DEFAULT 'automatic',
 ADD COLUMN icon_name text NOT NULL DEFAULT '',
 ADD COLUMN icon_color text NOT NULL DEFAULT '',
 ADD COLUMN icon_emoji text NOT NULL DEFAULT '',
 ADD COLUMN icon_image_key text NOT NULL DEFAULT '',
 ADD COLUMN icon_image_sha256 text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_settings DROP COLUMN icon_kind, DROP COLUMN icon_name, DROP COLUMN icon_color, DROP COLUMN icon_emoji, DROP COLUMN icon_image_key, DROP COLUMN icon_image_sha256;
