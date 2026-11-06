-- +goose Up
CREATE TABLE workspace_items (
    organization_id text NOT NULL,
    work_item_id text NOT NULL,
    workspace_id text NOT NULL CHECK (btrim(workspace_id) <> ''),
    project_id text NOT NULL,
    created_at timestamptz NOT NULL,
    closed_at timestamptz,
    PRIMARY KEY (organization_id, work_item_id),
    UNIQUE (organization_id, workspace_id),
    FOREIGN KEY (organization_id, project_id, work_item_id)
        REFERENCES issues (organization_id, project_id, native_id)
);

ALTER TABLE workspace_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_items FORCE ROW LEVEL SECURITY;
REVOKE ALL ON workspace_items FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_items TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON workspace_items TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON workspace_items TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE workspace_items;
