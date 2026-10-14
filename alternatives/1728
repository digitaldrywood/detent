-- +goose Up
ALTER TABLE attempt_diff_files ADD COLUMN patch_object TEXT NOT NULL DEFAULT '';
ALTER TABLE attempt_diff_files ADD COLUMN patch_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE attempt_diff_files ADD COLUMN patch_size INTEGER NOT NULL DEFAULT 0 CHECK (patch_size >= 0);
ALTER TABLE attempt_diff_files ADD COLUMN patch_expired INTEGER NOT NULL DEFAULT 0 CHECK (patch_expired IN (0, 1));
CREATE INDEX attempt_diff_files_inline_idx ON attempt_diff_files(diff_id, position) WHERE patch <> '';
CREATE INDEX attempt_diff_files_object_idx ON attempt_diff_files(patch_object) WHERE patch_object <> '';
CREATE INDEX change_versions_attempt_idx ON change_versions(json_extract(record_json, '$.attempt_id'));
CREATE TABLE attempt_diff_body_migration (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  vacuum_pending INTEGER NOT NULL CHECK (vacuum_pending IN (0, 1))
);
INSERT INTO attempt_diff_body_migration VALUES (1, EXISTS(SELECT 1 FROM attempt_diff_files WHERE patch <> ''));

-- +goose Down
CREATE TABLE attempt_diff_body_downgrade_guard (value INTEGER CHECK (value = 0));
INSERT INTO attempt_diff_body_downgrade_guard VALUES (1);
