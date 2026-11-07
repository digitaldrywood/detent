-- +goose Up
ALTER TABLE onboarding_issue_intake ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
 ADD COLUMN attempts bigint NOT NULL DEFAULT 0;
CREATE INDEX github_intake_ready ON onboarding_issue_intake(organization_id,next_attempt_at,project_id) WHERE hub_owned AND batch_status IN ('discovering','importing');
CREATE TABLE github_webhook_receipts (
 organization_id text NOT NULL REFERENCES organizations(organization_id),
 delivery_id text NOT NULL,
 repository text NOT NULL,
 event_type text NOT NULL,
 action text NOT NULL,
 payload_digest text NOT NULL,
 payload_object_key text NOT NULL,
 payload_sha256 text NOT NULL,
 payload_bytes bigint NOT NULL,
 payload_content_bytes bigint NOT NULL,
 received_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','completed')),
 after_project text NOT NULL DEFAULT '',
 attempts bigint NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,delivery_id)
);
CREATE INDEX github_webhook_ready ON github_webhook_receipts(organization_id,next_attempt_at,delivery_id) WHERE status='pending';
CREATE INDEX github_webhook_expiry ON github_webhook_receipts(organization_id,expires_at,delivery_id) WHERE status='completed' AND payload_object_key<>'';
CREATE INDEX github_settings_repository ON project_settings(organization_id,lower(repository),project_id);
CREATE INDEX github_settings_checkout ON project_settings(organization_id,lower(checkout_repository),project_id);
ALTER TABLE github_webhook_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_webhook_receipts FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON github_webhook_receipts TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON github_webhook_receipts TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON github_webhook_receipts TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
DROP TABLE github_webhook_receipts;
DROP INDEX github_settings_repository;
DROP INDEX github_settings_checkout;
DROP INDEX github_intake_ready;
ALTER TABLE onboarding_issue_intake DROP COLUMN next_attempt_at, DROP COLUMN attempts;
