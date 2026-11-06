-- +goose Up
CREATE TABLE project_actions (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 id text NOT NULL,
 slot smallint NOT NULL CHECK (slot BETWEEN 0 AND 49),
 name text NOT NULL,
 keybinding text NOT NULL DEFAULT '',
 icon text NOT NULL DEFAULT '',
 preview_url text NOT NULL DEFAULT '',
 open_preview boolean NOT NULL DEFAULT false,
 run_on_worktree_creation boolean NOT NULL DEFAULT false,
 created_by text NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 command_object_key text NOT NULL,
 command_sha256 text NOT NULL,
 command_bytes bigint NOT NULL CHECK (command_bytes > 0),
 command_content_bytes bigint NOT NULL CHECK (command_content_bytes > 0),
 PRIMARY KEY (organization_id,project_id,id),
 UNIQUE (organization_id,project_id,slot),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX project_actions_keybinding_idx ON project_actions(organization_id,project_id,keybinding) WHERE keybinding<>'';
ALTER TABLE project_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_actions FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_actions TO detent_runtime;
GRANT SELECT ON project_actions TO detent_platform;
CREATE POLICY tenant_isolation ON project_actions TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON project_actions FOR SELECT TO detent_platform USING(true);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'project action records require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
