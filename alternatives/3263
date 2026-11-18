-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE lease_policy_bindings ADD COLUMN selector_runner_id text;
ALTER TABLE lease_policy_bindings ADD COLUMN selector_machine_id text;
ALTER TABLE lease_policy_bindings ADD COLUMN selector_tags text[];
ALTER TABLE lease_policy_bindings ADD CONSTRAINT lease_policy_binding_selector CHECK (
    (selector_tags IS NULL) = (selector_runner_id IS NULL)
    AND (selector_tags IS NULL) = (selector_machine_id IS NULL)
    AND (selector_tags IS NULL OR cardinality(selector_tags) <= 32 AND array_position(selector_tags, NULL) IS NULL)
) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE lease_policy_bindings DROP CONSTRAINT lease_policy_binding_selector;
ALTER TABLE lease_policy_bindings DROP COLUMN selector_tags;
ALTER TABLE lease_policy_bindings DROP COLUMN selector_machine_id;
ALTER TABLE lease_policy_bindings DROP COLUMN selector_runner_id;
