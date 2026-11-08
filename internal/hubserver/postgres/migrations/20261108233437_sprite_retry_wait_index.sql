-- +goose Up
DROP INDEX sprite_provision_source_failures;
CREATE INDEX sprite_provision_source_failures ON sprite_lifecycle_operations(organization_id,project_id,source_revision,updated_at DESC,idempotency_key DESC) WHERE outcome='rejected' AND operation IN ('provision','bootstrap','checkpoint') AND next_attempt_at>updated_at;

-- +goose Down
DROP INDEX sprite_provision_source_failures;
CREATE INDEX sprite_provision_source_failures ON sprite_lifecycle_operations(organization_id,project_id,source_revision,updated_at DESC,idempotency_key DESC) WHERE outcome='rejected' AND operation IN ('provision','bootstrap','checkpoint');
