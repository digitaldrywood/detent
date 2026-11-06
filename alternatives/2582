-- +goose Up
CREATE TABLE artifact_references (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    service_id text NOT NULL,
    artifact_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    manifest_id text NOT NULL UNIQUE,
    schema_version smallint NOT NULL CHECK (schema_version = 1),
    run_id text NOT NULL DEFAULT '',
    attempt_id text NOT NULL DEFAULT '',
    version_id text NOT NULL DEFAULT '',
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    kind text NOT NULL CHECK (kind IN ('log','diff','screenshot','video')),
    state text NOT NULL CHECK (state IN ('partial','complete','interrupted')),
    availability text NOT NULL CHECK (availability = 'available'),
    bytes bigint NOT NULL CHECK (bytes >= 0 AND bytes <= 268435456),
    objects integer NOT NULL CHECK (objects >= 0 AND objects <= 1024),
    expires_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, artifact_id, revision),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id)
);
CREATE INDEX artifact_references_item_latest_idx ON artifact_references (organization_id, project_id, work_item_id, artifact_id, revision DESC);
CREATE TRIGGER artifact_references_immutable BEFORE UPDATE OR DELETE ON artifact_references FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

ALTER TABLE artifact_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_references FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_references FROM PUBLIC;
GRANT SELECT, INSERT ON artifact_references TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON artifact_references TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON artifact_references TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP INDEX artifact_references_item_latest_idx;
DROP TABLE artifact_references;
