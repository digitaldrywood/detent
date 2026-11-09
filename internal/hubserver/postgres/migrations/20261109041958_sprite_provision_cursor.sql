-- +goose Up
ALTER TABLE organization_sprite_pools
    ADD COLUMN provision_after_requested_at timestamptz NOT NULL DEFAULT '1970-01-01 00:00:00+00',
    ADD COLUMN provision_after_project_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE organization_sprite_pools
    DROP COLUMN provision_after_project_id,
    DROP COLUMN provision_after_requested_at;
