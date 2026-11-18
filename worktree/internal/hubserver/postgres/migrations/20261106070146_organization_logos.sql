-- +goose Up
ALTER TABLE organizations ADD COLUMN logo_image_key text NOT NULL DEFAULT '', ADD COLUMN logo_image_sha256 text NOT NULL DEFAULT '', ADD COLUMN logo_revision bigint NOT NULL DEFAULT 1;
ALTER TABLE attachments ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE attachments ADD CONSTRAINT attachments_organization_logo CHECK (project_id IS NOT NULL OR (retention_class='organization' AND name='organization-logo.png' AND work_item_id IS NULL AND comment_id IS NULL AND evidence_attempt_id IS NULL));

-- +goose Down
ALTER TABLE attachments ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE attachments DROP CONSTRAINT attachments_organization_logo;
ALTER TABLE organizations DROP COLUMN logo_image_key, DROP COLUMN logo_image_sha256, DROP COLUMN logo_revision;
