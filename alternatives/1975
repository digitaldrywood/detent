-- +goose Up
ALTER TABLE organizations ADD COLUMN logo_image_key TEXT;
ALTER TABLE organizations ADD COLUMN logo_image_sha256 TEXT;
ALTER TABLE organizations ADD COLUMN logo_revision INTEGER NOT NULL DEFAULT 1 CHECK(logo_revision > 0);
ALTER TABLE attachments ALTER COLUMN project_id DROP NOT NULL;

-- +goose Down
ALTER TABLE attachments ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE organizations DROP COLUMN logo_revision;
ALTER TABLE organizations DROP COLUMN logo_image_sha256;
ALTER TABLE organizations DROP COLUMN logo_image_key;
