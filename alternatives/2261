-- +goose Up
CREATE TABLE collaboration_events (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    id text NOT NULL DEFAULT '',
    sequence bigint NOT NULL DEFAULT 1 CHECK (sequence >= 1),
    type text NOT NULL DEFAULT '',
    schema_version bigint NOT NULL CHECK (schema_version = 1),
    actor_kind text NOT NULL DEFAULT '',
    actor_principal_id text NOT NULL DEFAULT '',
    event_revision bigint NOT NULL DEFAULT 0 CHECK (event_revision >= 0),
    change_id text NOT NULL DEFAULT '',
    version_id text NOT NULL DEFAULT '',
    head_sha text NOT NULL DEFAULT '',
    run_id text NOT NULL DEFAULT '',
    attempt_id text NOT NULL DEFAULT '',
    from_state text NOT NULL DEFAULT '',
    to_state text NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, project_id, work_item_id, sequence),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id)
);

CREATE TABLE collaboration_versions (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    record_id text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, record_id, revision),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id)
);

CREATE TABLE change_requests (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    id text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
    title text NOT NULL DEFAULT '',
    current_version_id text,
    evidence_sequence bigint NOT NULL DEFAULT 0 CHECK (evidence_sequence >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, project_id, id),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id)
);

CREATE TABLE change_issue_links (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    change_id text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id, change_id, work_item_id),
    FOREIGN KEY (organization_id, project_id, change_id) REFERENCES change_requests (organization_id, project_id, id),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id)
);

CREATE TABLE change_versions (
    organization_id text NOT NULL,
    id text NOT NULL DEFAULT '',
    change_id text NOT NULL DEFAULT '',
    number bigint NOT NULL DEFAULT 1 CHECK (number >= 1),
    head_sha text NOT NULL DEFAULT '',
    base_sha text NOT NULL DEFAULT '',
    merge_base_sha text NOT NULL DEFAULT '',
    repository text NOT NULL DEFAULT '',
    policy_revision_id text NOT NULL DEFAULT '',
    attempt_id text NOT NULL DEFAULT '',
    actor_kind text NOT NULL DEFAULT '',
    actor_principal_id text NOT NULL DEFAULT '',
    external_provider text NOT NULL DEFAULT '',
    external_id text NOT NULL DEFAULT '',
    external_url text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, change_id, id),
    UNIQUE (organization_id, change_id, number),
    FOREIGN KEY (organization_id, change_id) REFERENCES change_requests (organization_id, id)
);

CREATE TABLE change_evidence (
    organization_id text NOT NULL,
    change_id text NOT NULL DEFAULT '',
    sequence bigint NOT NULL DEFAULT 1 CHECK (sequence >= 1),
    version_id text,
    kind text NOT NULL CHECK (kind IN ('review', 'check', 'discussion')),
    source_key text,
    actor_kind text NOT NULL DEFAULT '',
    actor_principal_id text NOT NULL DEFAULT '',
    decision text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, change_id, sequence),
    UNIQUE (organization_id, change_id, kind, source_key),
    FOREIGN KEY (organization_id, change_id) REFERENCES change_requests (organization_id, id),
    FOREIGN KEY (organization_id, change_id, version_id) REFERENCES change_versions (organization_id, change_id, id)
);

CREATE TABLE native_commands (
    organization_id text NOT NULL,
    actor_id text NOT NULL,
    operation text NOT NULL,
    command_key text NOT NULL,
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payload_object_key text NOT NULL CHECK (payload_object_key = 'orgs/' || organization_id || '/blobs/sha256/' || substr(payload_sha256, 1, 2) || '/' || payload_sha256),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0 AND payload_bytes <= 33554432),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0 AND payload_content_bytes <= 33554432),
    PRIMARY KEY (organization_id, actor_id, operation, command_key)
);

ALTER TABLE change_requests ADD CONSTRAINT change_current_version_fk FOREIGN KEY (organization_id, id, current_version_id) REFERENCES change_versions (organization_id, change_id, id) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX change_requests_scope_idx ON change_requests (organization_id, project_id, work_item_id, id);
CREATE INDEX collaboration_versions_scope_idx ON collaboration_versions (organization_id, project_id, work_item_id);
CREATE INDEX change_issue_links_issue_idx ON change_issue_links (organization_id, project_id, work_item_id);
-- +goose StatementBegin
CREATE FUNCTION reject_event_change_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'history is immutable';
END;
$$;
-- +goose StatementEnd
ALTER TABLE collaboration_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE collaboration_events FORCE ROW LEVEL SECURITY;
REVOKE ALL ON collaboration_events FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON collaboration_events TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON collaboration_events TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON collaboration_events TO detent_platform USING (true) WITH CHECK (true);
CREATE TRIGGER collaboration_events_immutable BEFORE UPDATE OR DELETE ON collaboration_events FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
ALTER TABLE collaboration_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE collaboration_versions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON collaboration_versions FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON collaboration_versions TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON collaboration_versions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON collaboration_versions TO detent_platform USING (true) WITH CHECK (true);
CREATE TRIGGER collaboration_versions_immutable BEFORE UPDATE OR DELETE ON collaboration_versions FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
ALTER TABLE change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_requests FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_requests FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON change_requests TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_requests TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_requests TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE change_issue_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_issue_links FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_issue_links FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON change_issue_links TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_issue_links TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_issue_links TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE change_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_versions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_versions FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON change_versions TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_versions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_versions TO detent_platform USING (true) WITH CHECK (true);
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OR DELETE ON change_versions FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
ALTER TABLE change_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_evidence FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_evidence FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON change_evidence TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_evidence TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_evidence TO detent_platform USING (true) WITH CHECK (true);
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE OR DELETE ON change_evidence FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

ALTER TABLE native_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_commands FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_commands FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_commands TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_commands TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_commands TO detent_platform USING (true) WITH CHECK (true);
CREATE TRIGGER native_commands_immutable BEFORE UPDATE OR DELETE ON native_commands FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

-- +goose Down
DROP TABLE native_commands;
ALTER TABLE change_requests DROP CONSTRAINT change_current_version_fk;
DROP TABLE change_evidence;
DROP TABLE change_versions;
DROP TABLE change_issue_links;
DROP TABLE change_requests;
DROP TABLE collaboration_versions;
DROP TABLE collaboration_events;
DROP FUNCTION reject_event_change_mutation();
