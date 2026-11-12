-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE health_scheduled_reports (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 repository text NOT NULL CHECK (repository ~ '^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$'),
 run_id text NOT NULL CHECK (run_id ~ '^[1-9][0-9]{0,19}$'),
 run_attempt text NOT NULL CHECK (run_attempt ~ '^[1-9][0-9]{0,19}$'),
 run_url text NOT NULL CHECK (run_url='https://github.com/'||repository||'/actions/runs/'||run_id||'/attempts/'||run_attempt),
 head_sha text NOT NULL CHECK (head_sha ~ '^([a-f0-9]{40}|[a-f0-9]{64})$'),
 conclusion text NOT NULL CHECK (conclusion IN ('success','failure','skipped')),
 recorded_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,repository,run_id,run_attempt),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE INDEX health_scheduled_reports_time ON health_scheduled_reports(organization_id,project_id,recorded_at DESC,run_id DESC,run_attempt DESC);
CREATE INDEX health_scheduled_reports_conclusion ON health_scheduled_reports(organization_id,project_id,conclusion,recorded_at DESC,run_id DESC,run_attempt DESC);
ALTER TABLE health_scheduled_reports OWNER TO detent_migration;
ALTER TABLE health_scheduled_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE health_scheduled_reports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON health_scheduled_reports FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT ON health_scheduled_reports TO detent_runtime;
CREATE POLICY tenant_isolation ON health_scheduled_reports TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE health_scheduled_reports;
