-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE bound_name text;
BEGIN
 SELECT conname INTO STRICT bound_name FROM pg_constraint
 WHERE conrelid='organization_sprite_pools'::regclass AND contype='c'
 AND pg_get_constraintdef(oid) LIKE '%max_runners <= 100%';
 EXECUTE format('ALTER TABLE organization_sprite_pools DROP CONSTRAINT %I',bound_name);
END $$;
-- +goose StatementEnd
ALTER TABLE organization_sprite_pools ADD CONSTRAINT organization_sprite_pools_max_runners_check CHECK (max_runners>=min_runners);
CREATE INDEX sprite_members_active_created ON runner_sprite_memberships(organization_id,created_at,name) WHERE state<>'deleted';
CREATE INDEX sprite_members_deleted_created ON runner_sprite_memberships(organization_id,created_at DESC,name DESC) WHERE state='deleted';
CREATE INDEX sprite_members_active_allocation ON runner_sprite_memberships(organization_id,allocation_project_id) WHERE state<>'deleted';

-- +goose Down
DROP INDEX sprite_members_active_allocation;
DROP INDEX sprite_members_deleted_created;
DROP INDEX sprite_members_active_created;
ALTER TABLE organization_sprite_pools DROP CONSTRAINT organization_sprite_pools_max_runners_check;
ALTER TABLE organization_sprite_pools ADD CONSTRAINT organization_sprite_pools_max_runners_check CHECK (max_runners>=min_runners AND max_runners<=100) NOT VALID;
