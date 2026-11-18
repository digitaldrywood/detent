-- +goose Up
CREATE TABLE policy_revisions (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    policy_id text NOT NULL,
    approved_by text NOT NULL,
    approved_at timestamptz NOT NULL,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, project_id, policy_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE project_policies (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    policy_id text NOT NULL,
    PRIMARY KEY (organization_id, project_id),
    FOREIGN KEY (organization_id, project_id, policy_id) REFERENCES policy_revisions (organization_id, project_id, policy_id)
);

CREATE TABLE change_review_policy_revisions (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    review_policy_id text NOT NULL,
    policy_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, project_id, review_policy_id),
    UNIQUE (organization_id, project_id, revision),
    FOREIGN KEY (organization_id, project_id, policy_id) REFERENCES policy_revisions (organization_id, project_id, policy_id)
);

CREATE TABLE change_review_policies (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    review_policy_id text NOT NULL,
    policy_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    PRIMARY KEY (organization_id, project_id),
    FOREIGN KEY (organization_id, project_id, review_policy_id) REFERENCES change_review_policy_revisions (organization_id, project_id, review_policy_id),
    FOREIGN KEY (organization_id, project_id, policy_id) REFERENCES policy_revisions (organization_id, project_id, policy_id)
);

ALTER TABLE change_versions ADD COLUMN implementing_session_id bigint NOT NULL DEFAULT -1;
ALTER TABLE change_versions ADD COLUMN implementing_machine_id text NOT NULL DEFAULT '';

ALTER TABLE change_issue_links ADD COLUMN created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP;
UPDATE change_issue_links l SET created_at=c.created_at FROM change_requests c WHERE c.organization_id=l.organization_id AND c.project_id=l.project_id AND c.id=l.change_id;
CREATE INDEX change_issue_links_latest_issue_idx ON change_issue_links (organization_id, project_id, work_item_id, created_at DESC, change_id DESC);

CREATE TABLE change_landing_receipts (
    organization_id text NOT NULL,
    attempt_id text NOT NULL CHECK (attempt_id <> ''),
    version_id text NOT NULL,
    merge_sha text NOT NULL CHECK (merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, version_id) REFERENCES change_versions (organization_id, id),
    FOREIGN KEY (organization_id, attempt_id) REFERENCES native_attempts (organization_id, id)
);
CREATE INDEX change_landing_receipts_version_idx ON change_landing_receipts (organization_id, version_id, attempt_id);
CREATE TRIGGER change_landing_receipts_immutable BEFORE UPDATE OR DELETE ON change_landing_receipts FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

CREATE TRIGGER policy_revisions_immutable BEFORE UPDATE OR DELETE ON policy_revisions FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
CREATE TRIGGER change_review_policy_revisions_immutable BEFORE UPDATE OR DELETE ON change_review_policy_revisions FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

ALTER TABLE policy_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_revisions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON policy_revisions FROM PUBLIC;
GRANT SELECT, INSERT ON policy_revisions TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON policy_revisions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON policy_revisions TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE project_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_policies FORCE ROW LEVEL SECURITY;
REVOKE ALL ON project_policies FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON project_policies TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON project_policies TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON project_policies TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE change_review_policy_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_review_policy_revisions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_review_policy_revisions FROM PUBLIC;
GRANT SELECT, INSERT ON change_review_policy_revisions TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_review_policy_revisions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_review_policy_revisions TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE change_review_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_review_policies FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_review_policies FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON change_review_policies TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_review_policies TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_review_policies TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE change_landing_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_landing_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_landing_receipts FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON change_landing_receipts TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_landing_receipts TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_landing_receipts TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE project_policies;
DROP TABLE change_landing_receipts;
DROP INDEX change_issue_links_latest_issue_idx;
ALTER TABLE change_issue_links DROP COLUMN created_at;
ALTER TABLE change_versions DROP COLUMN implementing_machine_id;
ALTER TABLE change_versions DROP COLUMN implementing_session_id;
DROP TABLE change_review_policies;
DROP TABLE change_review_policy_revisions;
DROP TABLE policy_revisions;
