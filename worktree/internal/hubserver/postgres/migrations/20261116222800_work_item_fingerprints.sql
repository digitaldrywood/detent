-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE native_work_item_fingerprints (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('item','comment')),
 record_id text NOT NULL,
 fingerprint_digest text NOT NULL CHECK (fingerprint_digest ~ '^[0-9a-f]{64}$'),
 is_origin boolean NOT NULL,
 origin_kind text NOT NULL,
 release_series text NOT NULL DEFAULT '',
 PRIMARY KEY (organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest)
);
CREATE INDEX native_work_item_fingerprints_origin ON native_work_item_fingerprints(organization_id,project_id,fingerprint_digest,release_series,work_item_id) WHERE is_origin;
CREATE INDEX native_work_item_fingerprints_series ON native_work_item_fingerprints(organization_id,project_id,fingerprint_digest,release_series,work_item_id);
CREATE INDEX native_work_item_fingerprints_worker ON native_work_item_fingerprints(organization_id,project_id,fingerprint_digest,release_series,origin_kind,work_item_id);
CREATE INDEX native_work_item_fingerprints_lookup ON native_work_item_fingerprints(organization_id,project_id,fingerprint_digest,work_item_id);
ALTER TABLE native_work_item_fingerprints ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_fingerprints FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,DELETE ON native_work_item_fingerprints TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON native_work_item_fingerprints TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
ALTER TABLE native_work_item_page_progress
 ADD COLUMN fingerprint_phase smallint NOT NULL DEFAULT 0,
 ADD COLUMN fingerprint_issue_id bigint NOT NULL DEFAULT 0,
 ADD COLUMN fingerprint_comment_id text NOT NULL DEFAULT '';

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_work_item_page_progress DROP COLUMN IF EXISTS fingerprint_phase,DROP COLUMN IF EXISTS fingerprint_issue_id,DROP COLUMN IF EXISTS fingerprint_comment_id;
DROP TABLE IF EXISTS native_work_item_fingerprints;
