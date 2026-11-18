-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE attempt_diffs ADD COLUMN body_references_indexed boolean NOT NULL DEFAULT false;
CREATE TABLE attempt_diff_body_references (
    organization_id text NOT NULL,
    diff_id text NOT NULL,
    object_key text NOT NULL CHECK (btrim(object_key) <> ''),
    PRIMARY KEY (organization_id, diff_id, object_key),
    FOREIGN KEY (organization_id, diff_id) REFERENCES attempt_diffs (organization_id, id) ON DELETE CASCADE
);
CREATE INDEX attempt_diff_body_references_key_idx ON attempt_diff_body_references (organization_id, object_key);
ALTER TABLE attempt_diff_body_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_diff_body_references FORCE ROW LEVEL SECURITY;
REVOKE ALL ON attempt_diff_body_references FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON attempt_diff_body_references TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON attempt_diff_body_references TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON attempt_diff_body_references TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE attempt_diff_body_references;
ALTER TABLE attempt_diffs DROP COLUMN body_references_indexed;
