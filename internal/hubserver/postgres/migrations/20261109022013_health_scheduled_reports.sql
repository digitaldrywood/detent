-- +goose Up
CREATE TABLE health_scheduled_reports (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 repository text NOT NULL,
 run_id text NOT NULL,
 run_attempt text NOT NULL,
 run_url text NOT NULL,
 head_sha text NOT NULL,
 conclusion text NOT NULL CHECK (conclusion IN ('success','failure','skipped')),
 recorded_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,repository,run_id,run_attempt),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE INDEX health_scheduled_reports_time ON health_scheduled_reports(organization_id,project_id,recorded_at DESC,run_id DESC,run_attempt DESC);
CREATE INDEX health_scheduled_reports_conclusion ON health_scheduled_reports(organization_id,project_id,conclusion,recorded_at DESC);
ALTER TABLE health_scheduled_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE health_scheduled_reports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON health_scheduled_reports FROM PUBLIC;
GRANT SELECT,INSERT ON health_scheduled_reports TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON health_scheduled_reports TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON health_scheduled_reports TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
DROP TABLE health_scheduled_reports;
