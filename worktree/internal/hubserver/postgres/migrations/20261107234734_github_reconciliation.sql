-- +goose Up
CREATE TABLE github_reconciliation (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 repository text NOT NULL,
 node_id text NOT NULL,
 after_work_item text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1,
 attempts bigint NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE INDEX github_reconciliation_ready ON github_reconciliation(organization_id,next_attempt_at,project_id);
ALTER TABLE github_reconciliation ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_reconciliation FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON github_reconciliation TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON github_reconciliation TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON github_reconciliation TO detent_platform USING(true) WITH CHECK(true);


-- +goose Down
DROP TABLE github_reconciliation;
