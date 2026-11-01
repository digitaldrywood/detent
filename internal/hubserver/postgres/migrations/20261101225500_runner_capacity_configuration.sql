-- +goose Up
ALTER TABLE runner_identities
    ADD COLUMN capacity_config_revision text,
    ADD COLUMN capacity_config_local_limit integer,
    ADD COLUMN capacity_config_client_limit integer,
    ADD COLUMN capacity_config_runtime_limit integer,
    ADD COLUMN capacity_config_manageable boolean,
    ADD COLUMN capacity_config_constraint text,
    ADD COLUMN capacity_config_observed_at timestamptz,
    ADD CONSTRAINT runner_capacity_configuration_complete CHECK (
        (capacity_config_revision IS NULL AND capacity_config_local_limit IS NULL AND capacity_config_client_limit IS NULL AND capacity_config_runtime_limit IS NULL AND capacity_config_manageable IS NULL AND capacity_config_constraint IS NULL AND capacity_config_observed_at IS NULL)
        OR
        (capacity_config_revision ~ '^[0-9a-f]{64}$' AND capacity_config_local_limit BETWEEN 1 AND 10000 AND capacity_config_client_limit BETWEEN 1 AND 10000 AND capacity_config_runtime_limit BETWEEN 1 AND 10000 AND capacity_config_manageable IS NOT NULL AND capacity_config_constraint IS NOT NULL AND capacity_config_observed_at IS NOT NULL)
    );

-- +goose Down
ALTER TABLE runner_identities
    DROP CONSTRAINT runner_capacity_configuration_complete,
    DROP COLUMN capacity_config_observed_at,
    DROP COLUMN capacity_config_constraint,
    DROP COLUMN capacity_config_manageable,
    DROP COLUMN capacity_config_runtime_limit,
    DROP COLUMN capacity_config_client_limit,
    DROP COLUMN capacity_config_local_limit,
    DROP COLUMN capacity_config_revision;
