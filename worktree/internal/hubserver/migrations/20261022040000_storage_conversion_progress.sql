-- +goose Up
CREATE TABLE storage_conversion_progress (
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    family TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    format_version INTEGER NOT NULL,
    through_rowid INTEGER NOT NULL,
    cursor_rowid INTEGER NOT NULL DEFAULT 0,
    converted_rows INTEGER NOT NULL DEFAULT 0,
    reclaimed_bytes INTEGER NOT NULL DEFAULT 0,
    complete INTEGER NOT NULL DEFAULT 0 CHECK (complete IN (0,1)),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (organization_id, project_id, family, aggregate_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id,id)
);

CREATE TABLE attempt_diff_file_archives (
    diff_id TEXT NOT NULL REFERENCES attempt_diffs(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    object_key TEXT NOT NULL,
    object_sha256 TEXT NOT NULL,
    object_bytes INTEGER NOT NULL,
    content_bytes INTEGER NOT NULL,
    PRIMARY KEY (diff_id,position)
);

-- +goose Down
DROP TABLE attempt_diff_file_archives;
DROP TABLE storage_conversion_progress;
