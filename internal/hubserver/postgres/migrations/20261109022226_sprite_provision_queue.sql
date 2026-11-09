-- +goose Up
CREATE INDEX sprite_allocations_provision_queue ON project_sprite_allocations(organization_id,requested_at,project_id) WHERE desired_count>0;

-- +goose Down
DROP INDEX sprite_allocations_provision_queue;
