-- +goose Up
CREATE TABLE scheduled_ci_decisions (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 repository text NOT NULL,
 head_sha text NOT NULL,
 run_id text NOT NULL,
 run_attempt text NOT NULL,
 job_id text NOT NULL,
 outcome text NOT NULL CHECK (outcome IN ('source','infrastructure','unknown')),
 target text NOT NULL,
 reason text NOT NULL,
 created_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,repository,run_id,run_attempt,job_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
ALTER TABLE scheduled_ci_decisions OWNER TO detent_migration;
REVOKE ALL ON scheduled_ci_decisions FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT ON scheduled_ci_decisions TO detent_runtime;
ALTER TABLE scheduled_ci_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE scheduled_ci_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON scheduled_ci_decisions TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'scheduled decisions require an explicit retained-data migration';
END $$;
-- +goose StatementEnd
