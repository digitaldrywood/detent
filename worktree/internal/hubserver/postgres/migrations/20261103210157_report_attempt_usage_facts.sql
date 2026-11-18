-- +goose Up
CREATE TABLE attempt_usage (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 attempt_id text NOT NULL,
 period timestamptz NOT NULL,
 provider text NOT NULL,
 model text NOT NULL,
 input bigint NOT NULL CHECK(input>=0),
 cached_input bigint NOT NULL CHECK(cached_input>=0),
 output bigint NOT NULL CHECK(output>=0),
 cost_estimate double precision NOT NULL CHECK(cost_estimate>=0 AND cost_estimate<'Infinity'::double precision),
 currency text NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,attempt_id,period,provider,model),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE,
 FOREIGN KEY(organization_id,attempt_id) REFERENCES native_attempts(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX attempt_usage_report_window_idx ON attempt_usage(organization_id,project_id,period,attempt_id);
ALTER TABLE attempt_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_usage FORCE ROW LEVEL SECURITY;
REVOKE ALL ON attempt_usage FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON attempt_usage TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON attempt_usage TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON attempt_usage TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
DROP TABLE attempt_usage;
