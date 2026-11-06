-- +goose Up
CREATE TABLE github_imports (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 id text NOT NULL,
 issue_number bigint NOT NULL CHECK (issue_number > 0),
 work_item_id text NOT NULL DEFAULT '',
 stage text NOT NULL,
 cursor text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1,
 pages bigint NOT NULL DEFAULT 0,
 status text NOT NULL,
 gaps text[] NOT NULL DEFAULT '{}',
 last_error text NOT NULL DEFAULT '',
 retry_after timestamptz,
 source_updated_at timestamptz,
 observed_at timestamptz NOT NULL,
 intake_pending boolean NOT NULL DEFAULT false,
 edit_sequence bigint NOT NULL DEFAULT 0,
 PRIMARY KEY (organization_id, project_id, id),
 UNIQUE (organization_id, project_id, issue_number),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);
ALTER TABLE github_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_imports FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON github_imports TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON github_imports TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON github_imports TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE github_imports;
