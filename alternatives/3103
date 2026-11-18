-- +goose Up
CREATE TABLE project_budget_overrides (
 organization_id text NOT NULL,project_id text NOT NULL,
 per_day_max_usd double precision CHECK(per_day_max_usd>0 AND per_day_max_usd<'Infinity'::double precision),
 per_issue_max_usd double precision CHECK(per_issue_max_usd>0 AND per_issue_max_usd<'Infinity'::double precision),
 expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL,reason text NOT NULL CHECK(btrim(reason)<>''),revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 PRIMARY KEY(organization_id,project_id),FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id),
 CHECK(per_day_max_usd IS NOT NULL OR per_issue_max_usd IS NOT NULL),CHECK(expires_at>created_at)
);
CREATE INDEX budget_overrides_active ON project_budget_overrides(organization_id,expires_at,project_id);

ALTER TABLE project_budget_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_budget_overrides FORCE ROW LEVEL SECURITY;
REVOKE ALL ON project_budget_overrides FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_budget_overrides TO detent_runtime;
CREATE POLICY tenant_isolation ON project_budget_overrides TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE project_budget_overrides;
