-- +goose Up
CREATE TABLE health_evaluation_work (
 organization_id text PRIMARY KEY REFERENCES organizations(organization_id) ON DELETE CASCADE,
 id text NOT NULL,
 evaluated_at timestamptz NOT NULL,
 input_object_key text NOT NULL,
 input_sha256 text NOT NULL CHECK(length(input_sha256)=64),
 input_bytes bigint NOT NULL CHECK(input_bytes>0 AND input_bytes<=33554432),
 input_content_bytes bigint NOT NULL CHECK(input_content_bytes>0 AND input_content_bytes<=33554432),
 finding_count bigint NOT NULL CHECK(finding_count>=0 AND finding_count<=10000),
 baseline_count bigint NOT NULL CHECK(baseline_count>=0 AND baseline_count<=10000),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 phase text NOT NULL DEFAULT 'publish' CHECK(phase IN('publish','resolve','baseline_clear','baseline_put')),
 position bigint NOT NULL DEFAULT 0 CHECK(position>=0 AND position<=10000),
 finding_cursor text NOT NULL DEFAULT '',
 baseline_project_cursor text NOT NULL DEFAULT '',
 baseline_signal_cursor text NOT NULL DEFAULT '',
 baseline_lane_cursor text NOT NULL DEFAULT ''
);
ALTER TABLE health_evaluation_work OWNER TO detent_migration;
REVOKE ALL ON health_evaluation_work FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON health_evaluation_work TO detent_runtime;
ALTER TABLE health_evaluation_work ENABLE ROW LEVEL SECURITY;
ALTER TABLE health_evaluation_work FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON health_evaluation_work TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE health_evaluation_work;
