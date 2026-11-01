-- +goose Up
ALTER TABLE projects ADD COLUMN icon_kind TEXT CHECK (icon_kind IN ('automatic','lucide','emoji','image'));
ALTER TABLE projects ADD COLUMN icon_name TEXT;
ALTER TABLE projects ADD COLUMN icon_color TEXT;
ALTER TABLE projects ADD COLUMN icon_emoji TEXT;
ALTER TABLE projects ADD COLUMN icon_image_key TEXT;
ALTER TABLE projects ADD COLUMN icon_image_sha256 TEXT;

-- +goose Down
ALTER TABLE projects DROP COLUMN icon_image_sha256;
ALTER TABLE projects DROP COLUMN icon_image_key;
ALTER TABLE projects DROP COLUMN icon_emoji;
ALTER TABLE projects DROP COLUMN icon_color;
ALTER TABLE projects DROP COLUMN icon_name;
ALTER TABLE projects DROP COLUMN icon_kind;
