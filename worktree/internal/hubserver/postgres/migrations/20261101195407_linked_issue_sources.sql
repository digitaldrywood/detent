-- +goose Up
ALTER TABLE change_versions ADD COLUMN source_object_key text;
ALTER TABLE change_versions ADD COLUMN source_sha256 text;
ALTER TABLE change_versions ADD COLUMN source_bytes bigint;
ALTER TABLE change_versions ADD COLUMN source_content_bytes bigint;
ALTER TABLE change_versions ADD CONSTRAINT change_version_source_ref_check CHECK ((source_object_key IS NULL AND source_sha256 IS NULL AND source_bytes IS NULL AND source_content_bytes IS NULL) OR (source_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(source_sha256, 1, 2) || '/' || source_sha256 AND source_sha256 ~ '^[0-9a-f]{64}$' AND source_bytes > 0 AND source_content_bytes > 0));

CREATE TABLE linked_issue_sources (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    source_url text NOT NULL,
    title_supplied boolean NOT NULL,
    body_supplied boolean NOT NULL,
    title_owned boolean NOT NULL DEFAULT false,
    body_owned boolean NOT NULL DEFAULT false,
    snapshot_object_key text,
    snapshot_sha256 text,
    snapshot_bytes bigint,
    snapshot_content_bytes bigint,
    PRIMARY KEY (organization_id, work_item_id),
    UNIQUE (organization_id, project_id, source_url),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id) ON DELETE CASCADE,
    CHECK ((snapshot_object_key IS NULL AND snapshot_sha256 IS NULL AND snapshot_bytes IS NULL AND snapshot_content_bytes IS NULL) OR (snapshot_object_key IS NOT NULL AND snapshot_sha256 IS NOT NULL AND snapshot_bytes > 0 AND snapshot_content_bytes > 0))
);
ALTER TABLE linked_issue_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE linked_issue_sources FORCE ROW LEVEL SECURITY;
REVOKE ALL ON linked_issue_sources FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON linked_issue_sources TO detent_runtime;
CREATE POLICY tenant_isolation ON linked_issue_sources TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE linked_issue_sources;
ALTER TABLE change_versions DROP CONSTRAINT change_version_source_ref_check;
ALTER TABLE change_versions DROP COLUMN source_object_key;
ALTER TABLE change_versions DROP COLUMN source_sha256;
ALTER TABLE change_versions DROP COLUMN source_bytes;
ALTER TABLE change_versions DROP COLUMN source_content_bytes;
