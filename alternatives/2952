-- +goose Up
ALTER TABLE machines ADD COLUMN sprite_name text NOT NULL DEFAULT '';

CREATE TABLE runner_sprite_memberships (
    organization_id text NOT NULL,
    name text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 200),
    provider_organization text NOT NULL,
    token_project_id text NOT NULL DEFAULT '',
    enrollment_id text NOT NULL,
    state text NOT NULL CHECK (state IN ('bootstrapping','enrolled','deleting','deleted')),
    bootstrap_log text NOT NULL DEFAULT '',
    idle_since timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    allocation_project_id text NOT NULL DEFAULT '',
    allocation_revision bigint NOT NULL DEFAULT 0 CHECK (allocation_revision >= 0),
    PRIMARY KEY (organization_id,name),
    UNIQUE (organization_id,enrollment_id),
    FOREIGN KEY (organization_id,enrollment_id) REFERENCES runner_enrollments (organization_id,id) ON DELETE CASCADE
);

CREATE INDEX runner_sprite_memberships_state_idx ON runner_sprite_memberships (organization_id,state,name);
ALTER TABLE runner_sprite_memberships OWNER TO detent_migration;
REVOKE ALL ON runner_sprite_memberships FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_sprite_memberships TO detent_runtime;
ALTER TABLE runner_sprite_memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_sprite_memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_sprite_memberships TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE runner_sprite_memberships;
DROP INDEX IF EXISTS runner_sprite_memberships_state_idx;
ALTER TABLE machines DROP COLUMN sprite_name;
