-- +goose Up
CREATE TABLE change_content_objects (
    organization_id TEXT NOT NULL,
    object_sha256 TEXT NOT NULL,
    object_key TEXT NOT NULL,
    object_bytes INTEGER NOT NULL CHECK (object_bytes > 0),
    content_bytes INTEGER NOT NULL CHECK (content_bytes > 0),
    PRIMARY KEY (organization_id,object_sha256)
);
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_insert AFTER INSERT ON change_content_objects BEGIN
    UPDATE hosted_usage_counters SET value=value+NEW.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_delete AFTER DELETE ON change_content_objects BEGIN
    UPDATE hosted_usage_counters SET value=value-OLD.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd

ALTER TABLE change_versions ADD COLUMN policy_revision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE change_sources ADD COLUMN object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE change_sources ADD COLUMN object_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE change_sources ADD COLUMN object_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE change_sources ADD COLUMN content_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempt_diffs ADD COLUMN object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE attempt_diffs ADD COLUMN object_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE attempt_diffs ADD COLUMN object_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempt_diffs ADD COLUMN content_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempt_diffs ADD COLUMN manifest_expired INTEGER NOT NULL DEFAULT 0 CHECK (manifest_expired IN (0,1));
CREATE TABLE change_command_outputs (
    version_id TEXT NOT NULL REFERENCES change_versions(id) ON DELETE CASCADE,
    review_id TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL CHECK (role IN ('validation','validator','implementation_review')),
    position INTEGER NOT NULL CHECK (position >= 0),
    organization_id TEXT NOT NULL,
    object_key TEXT NOT NULL,
    object_sha256 TEXT NOT NULL,
    object_bytes INTEGER NOT NULL CHECK (object_bytes > 0),
    content_bytes INTEGER NOT NULL CHECK (content_bytes > 0),
    PRIMARY KEY (version_id, review_id, role, position)
);

-- +goose StatementBegin
CREATE TRIGGER change_sources_immutable_reference BEFORE UPDATE ON change_sources
WHEN NEW.object_key IS NOT OLD.object_key OR NEW.object_sha256 IS NOT OLD.object_sha256
 OR NEW.object_bytes IS NOT OLD.object_bytes OR NEW.content_bytes IS NOT OLD.content_bytes
BEGIN SELECT RAISE(ABORT,'change source reference is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_command_outputs_no_update BEFORE UPDATE ON change_command_outputs
BEGIN SELECT RAISE(ABORT,'command output is immutable'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER change_sources_immutable_reference;
DROP TRIGGER change_command_outputs_no_update;
DROP TRIGGER change_content_usage_delete;
DROP TRIGGER change_content_usage_insert;
DROP TABLE change_content_objects;
DROP TABLE change_command_outputs;
ALTER TABLE attempt_diffs DROP COLUMN manifest_expired;
ALTER TABLE attempt_diffs DROP COLUMN content_bytes;
ALTER TABLE attempt_diffs DROP COLUMN object_bytes;
ALTER TABLE attempt_diffs DROP COLUMN object_sha256;
ALTER TABLE attempt_diffs DROP COLUMN object_key;
ALTER TABLE change_sources DROP COLUMN content_bytes;
ALTER TABLE change_sources DROP COLUMN object_bytes;
ALTER TABLE change_sources DROP COLUMN object_sha256;
ALTER TABLE change_sources DROP COLUMN object_key;
ALTER TABLE change_versions DROP COLUMN policy_revision_id;
