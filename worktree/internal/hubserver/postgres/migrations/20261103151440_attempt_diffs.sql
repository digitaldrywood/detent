-- +goose Up
CREATE TABLE attempt_diffs (
    organization_id text NOT NULL,
    id text NOT NULL CHECK (btrim(id) <> ''),
    attempt_id text NOT NULL CHECK (btrim(attempt_id) <> ''),
    issue_id bigint NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    source text NOT NULL CHECK (source IN ('attempt', 'workspace')),
    source_id text NOT NULL DEFAULT '',
    sequence bigint NOT NULL CHECK (sequence > 0),
    base_sha text NOT NULL,
    head_sha text NOT NULL,
    producer_kind text NOT NULL CHECK (producer_kind IN ('attempt', 'workspace')),
    producer_id text NOT NULL,
    producer_runner_id text NOT NULL,
    producer_lease_id text NOT NULL,
    producer_fencing_token bigint NOT NULL CHECK (producer_fencing_token > 0),
    file_count integer NOT NULL CHECK (file_count >= 0),
    patch_bytes bigint NOT NULL CHECK (patch_bytes >= 0),
    posted_bytes bigint NOT NULL CHECK (posted_bytes >= 0),
    truncated boolean NOT NULL,
    archive_id text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, attempt_id, source, source_id, sequence),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id),
    FOREIGN KEY (organization_id, archive_id) REFERENCES archive_objects (organization_id, id)
);

CREATE INDEX attempt_diffs_latest_idx ON attempt_diffs (organization_id, attempt_id, source, sequence DESC);
CREATE INDEX attempt_diffs_item_idx ON attempt_diffs (organization_id, project_id, work_item_id, created_at DESC, id DESC);

ALTER TABLE attempt_diffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_diffs FORCE ROW LEVEL SECURITY;
REVOKE ALL ON attempt_diffs FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON attempt_diffs TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON attempt_diffs TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON attempt_diffs TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE attempt_diffs;
