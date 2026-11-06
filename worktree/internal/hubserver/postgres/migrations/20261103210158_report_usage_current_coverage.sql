-- +goose Up
CREATE INDEX cost_sources_report_attempt_idx ON cost_sources(organization_id,resource_id,source_key) WHERE bucket='runner_api';
CREATE TABLE report_usage_current (
 organization_id text NOT NULL,
 source_key text NOT NULL,
 revision bigint NOT NULL,
 project_id text NOT NULL,
 attempt_id text NOT NULL,
 period timestamptz NOT NULL,
 provider text NOT NULL,
 model text NOT NULL,
 input bigint NOT NULL CHECK(input>=0),
 cached_input bigint NOT NULL CHECK(cached_input>=0),
 output bigint NOT NULL CHECK(output>=0),
 cost_estimate double precision NOT NULL CHECK(cost_estimate>=0 AND cost_estimate<'Infinity'::double precision),
 complete boolean NOT NULL,
 PRIMARY KEY(organization_id,source_key),
 FOREIGN KEY(organization_id,source_key) REFERENCES cost_sources(organization_id,source_key) ON DELETE CASCADE,
 FOREIGN KEY(organization_id,attempt_id) REFERENCES native_attempts(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX report_usage_current_attempt_idx ON report_usage_current(organization_id,attempt_id,period);
CREATE TABLE report_usage_import_coverage (
 organization_id text NOT NULL,
 attempt_id text NOT NULL,
 source_sha256 text NOT NULL CHECK(length(source_sha256)=64),
 row_count bigint NOT NULL CHECK(row_count>0),
 PRIMARY KEY(organization_id,attempt_id),
 FOREIGN KEY(organization_id,attempt_id) REFERENCES native_attempts(organization_id,id) ON DELETE CASCADE
);
ALTER TABLE report_usage_current ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_usage_current FORCE ROW LEVEL SECURITY;
ALTER TABLE report_usage_import_coverage ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_usage_import_coverage FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON report_usage_current,report_usage_import_coverage TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON report_usage_current TO detent_runtime,detent_migration USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON report_usage_current TO detent_platform USING(true) WITH CHECK(true);
CREATE POLICY tenant_isolation ON report_usage_import_coverage TO detent_runtime,detent_migration USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON report_usage_import_coverage TO detent_platform USING(true) WITH CHECK(true);

CREATE TABLE report_attempt_facts (
 organization_id text NOT NULL,
 attempt_id text NOT NULL,
 first_event_id text NOT NULL,
 latest_event_id text NOT NULL,
 outcome_event_id text,
 PRIMARY KEY(organization_id,attempt_id),
 FOREIGN KEY(organization_id,attempt_id) REFERENCES native_attempts(organization_id,id) ON DELETE CASCADE,
 FOREIGN KEY(organization_id,first_event_id) REFERENCES collaboration_events(organization_id,id) ON DELETE CASCADE,
 FOREIGN KEY(organization_id,latest_event_id) REFERENCES collaboration_events(organization_id,id) ON DELETE CASCADE,
 FOREIGN KEY(organization_id,outcome_event_id) REFERENCES collaboration_events(organization_id,id) ON DELETE CASCADE
);
ALTER TABLE report_attempt_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_attempt_facts FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON report_attempt_facts TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON report_attempt_facts TO detent_runtime,detent_migration USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON report_attempt_facts TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
DROP TABLE report_attempt_facts;
DROP TABLE report_usage_import_coverage;
DROP TABLE report_usage_current;
DROP INDEX cost_sources_report_attempt_idx;
