-- +goose Up
CREATE TABLE analytics_maintenance_cursor (
    id INTEGER PRIMARY KEY CHECK (id=1),
    projection_rowid INTEGER NOT NULL DEFAULT 0,
    sample_hour INTEGER NOT NULL DEFAULT -9223372036854775808,
    sample_metric TEXT NOT NULL DEFAULT '',
    sample_dimension TEXT NOT NULL DEFAULT '',
    sample_rowid INTEGER NOT NULL DEFAULT 0,
    samples_done INTEGER NOT NULL DEFAULT 0,
    residence_hour INTEGER NOT NULL DEFAULT -9223372036854775808,
    residence_organization TEXT NOT NULL DEFAULT '',
    residence_project TEXT NOT NULL DEFAULT '',
    residence_id TEXT NOT NULL DEFAULT ''
);
INSERT INTO analytics_maintenance_cursor(id) VALUES(1) ON CONFLICT(id) DO NOTHING;
CREATE INDEX analytics_projection_rowid_pending ON analytics_hourly_rollups(projection_version) WHERE projection_version IS NULL;
CREATE INDEX analytics_sample_compaction_order ON analytics_hourly_rollups(hour,metric,dimension) WHERE projection_version=1;

-- +goose Down
DROP INDEX analytics_sample_compaction_order;
DROP INDEX analytics_projection_rowid_pending;
DROP TABLE analytics_maintenance_cursor;
