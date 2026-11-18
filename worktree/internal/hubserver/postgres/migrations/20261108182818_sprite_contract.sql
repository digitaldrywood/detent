-- +goose Up
CREATE TABLE organization_sprite_pools (
 organization_id text PRIMARY KEY REFERENCES organizations(organization_id),
 min_runners integer NOT NULL CHECK (min_runners>=0),
 max_runners integer NOT NULL CHECK (max_runners>=min_runners AND max_runners<=100),
 idle_seconds integer NOT NULL CHECK (idle_seconds>=30),
 bootstrap_object_key text,
 configured_by text NOT NULL CHECK (btrim(configured_by)<>''),
 revision bigint NOT NULL CHECK (revision>0),
 isolation_tier text NOT NULL CHECK (isolation_tier IN ('sandbox','native-trusted')),
 share_idle_capacity boolean NOT NULL,
 placement_mode text NOT NULL CHECK (placement_mode IN ('blended','sprites_only','local_first')),
 todo_threshold integer NOT NULL,
 overflow_slots integer NOT NULL,
 max_sprite_slots integer NOT NULL CHECK (max_sprite_slots BETWEEN 0 AND 100),
 CHECK ((placement_mode='local_first' AND todo_threshold>0 AND overflow_slots BETWEEN 1 AND 100) OR (placement_mode<>'local_first' AND todo_threshold=0 AND overflow_slots=0))
);
CREATE TABLE project_sprite_allocations (
 organization_id text NOT NULL REFERENCES organization_sprite_pools(organization_id),
 project_id text NOT NULL,
 desired_count integer NOT NULL CHECK (desired_count>=0 AND desired_count<=100),
 revision bigint NOT NULL CHECK (revision>0),
 provision_failures bigint NOT NULL CHECK (provision_failures>=0),
 requested_at timestamptz NOT NULL,
 configured_by text NOT NULL CHECK (btrim(configured_by)<>''),
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE INDEX sprite_allocations_requested ON project_sprite_allocations(organization_id,requested_at,project_id);
CREATE INDEX sprite_members_allocation ON runner_sprite_memberships(organization_id,allocation_project_id,state,name);
CREATE TABLE sprite_lifecycle_operations (
 organization_id text NOT NULL,
 idempotency_key text NOT NULL CHECK (btrim(idempotency_key)<>''),
 name text NOT NULL,
 operation text NOT NULL CHECK (operation IN ('provision','wake','delete','bootstrap')),
 outcome text NOT NULL CHECK (outcome IN ('pending','committed','rejected','uncertain')),
 receipt_object_key text,
 attempts bigint NOT NULL CHECK (attempts>=0),
 next_attempt_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,idempotency_key),
 FOREIGN KEY (organization_id,name) REFERENCES runner_sprite_memberships(organization_id,name)
);
CREATE INDEX sprite_lifecycle_pending ON sprite_lifecycle_operations(organization_id,next_attempt_at,idempotency_key) WHERE outcome IN ('pending','uncertain');
ALTER TABLE organization_sprite_pools ENABLE ROW LEVEL SECURITY;
ALTER TABLE organization_sprite_pools FORCE ROW LEVEL SECURITY;
REVOKE ALL ON organization_sprite_pools FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON organization_sprite_pools TO detent_runtime;
CREATE POLICY tenant_isolation ON organization_sprite_pools TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE project_sprite_allocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_sprite_allocations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON project_sprite_allocations FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_sprite_allocations TO detent_runtime;
CREATE POLICY tenant_isolation ON project_sprite_allocations TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE sprite_lifecycle_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE sprite_lifecycle_operations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON sprite_lifecycle_operations FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON sprite_lifecycle_operations TO detent_runtime;
CREATE POLICY tenant_isolation ON sprite_lifecycle_operations TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE sprite_lifecycle_operations;
DROP INDEX sprite_members_allocation;
DROP TABLE project_sprite_allocations;
DROP TABLE organization_sprite_pools;
