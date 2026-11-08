-- +goose Up
ALTER TABLE sprite_lifecycle_operations ADD COLUMN source_revision text NOT NULL DEFAULT '';
CREATE INDEX sprite_provision_source_failures ON sprite_lifecycle_operations(organization_id,project_id,source_revision,updated_at DESC,idempotency_key DESC) WHERE outcome='rejected' AND operation IN ('provision','bootstrap','checkpoint');

-- +goose Down
DROP INDEX sprite_provision_source_failures;
ALTER TABLE sprite_lifecycle_operations DROP COLUMN source_revision;
