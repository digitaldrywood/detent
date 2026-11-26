-- +goose Up
CREATE TABLE scheduled_ci_classification_claims (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 job_key text NOT NULL,
 receipt_id text NOT NULL,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,job_key),
 UNIQUE (organization_id,receipt_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id,receipt_id) REFERENCES hosted_usage_receipts(organization_id,receipt_id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);
ALTER TABLE scheduled_ci_classification_claims OWNER TO detent_migration;
REVOKE ALL ON scheduled_ci_classification_claims FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON scheduled_ci_classification_claims TO detent_runtime;
ALTER TABLE scheduled_ci_classification_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE scheduled_ci_classification_claims FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON scheduled_ci_classification_claims TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

CREATE INDEX hosted_ci_usage_receipts_retention_idx ON hosted_usage_receipts (organization_id,window_start,receipt_id)
 WHERE starts_with(metric,'scheduled-ci/');
CREATE INDEX hosted_ci_usage_windows_retention_idx ON hosted_usage_windows (organization_id,window_start,metric)
 WHERE starts_with(metric,'scheduled-ci/');

-- +goose Down
DROP INDEX hosted_ci_usage_windows_retention_idx;
DROP INDEX hosted_ci_usage_receipts_retention_idx;
DROP TABLE scheduled_ci_classification_claims;
