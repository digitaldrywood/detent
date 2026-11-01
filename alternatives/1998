-- +goose Up
CREATE TABLE derived_refresh_revisions (
 work_item_id TEXT PRIMARY KEY,
 analytics_revision INTEGER NOT NULL DEFAULT 0,
 board_revision INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE analytics_rollup_positions ADD COLUMN refresh_revision INTEGER NOT NULL DEFAULT -1;
ALTER TABLE analytics_rollup_positions ADD COLUMN refresh_phase INTEGER NOT NULL DEFAULT -1;
ALTER TABLE analytics_rollup_positions ADD COLUMN refresh_offset INTEGER NOT NULL DEFAULT 0;
ALTER TABLE analytics_rollup_positions ADD COLUMN refresh_source TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN analytics_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE analytics_hourly_rollups ADD COLUMN source_revision INTEGER;
CREATE INDEX analytics_projection_source ON analytics_hourly_rollups(organization_id,project_id,work_item_id,dimension,source_revision) WHERE metric='attempt';
CREATE INDEX analytics_lease_source ON analytics_hourly_rollups(organization_id,project_id,dimension,acquired_at,released_at,expires_at,source_revision) WHERE metric='lease';
CREATE INDEX analytics_usage_source ON analytics_hourly_rollups(organization_id,project_id,attempt_id,period,provider,model,currency) WHERE metric='usage';
-- +goose StatementBegin
CREATE TRIGGER analytics_dirty_revision BEFORE INSERT ON analytics_rollup_dirty
BEGIN
 INSERT INTO derived_refresh_revisions(work_item_id,analytics_revision) VALUES(NEW.work_item_id,1)
 ON CONFLICT(work_item_id) DO UPDATE SET analytics_revision=analytics_revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER native_board_dirty_revision BEFORE INSERT ON native_board_dirty
BEGIN
 INSERT INTO derived_refresh_revisions(work_item_id,board_revision) VALUES(NEW.work_item_id,1)
 ON CONFLICT(work_item_id) DO UPDATE SET board_revision=board_revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_attempt_revision AFTER UPDATE ON native_attempts
WHEN NEW.analytics_revision=OLD.analytics_revision
BEGIN
 UPDATE native_attempts SET analytics_revision=OLD.analytics_revision+1 WHERE id=NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP INDEX analytics_lease_source;
DROP INDEX analytics_usage_source;
DROP INDEX analytics_projection_source;
DROP TRIGGER analytics_attempt_revision;
DROP TRIGGER native_board_dirty_revision;
DROP TRIGGER analytics_dirty_revision;
ALTER TABLE analytics_hourly_rollups DROP COLUMN source_revision;
ALTER TABLE native_attempts DROP COLUMN analytics_revision;
ALTER TABLE analytics_rollup_positions DROP COLUMN refresh_source;
ALTER TABLE analytics_rollup_positions DROP COLUMN refresh_offset;
ALTER TABLE analytics_rollup_positions DROP COLUMN refresh_phase;
ALTER TABLE analytics_rollup_positions DROP COLUMN refresh_revision;
DROP TABLE derived_refresh_revisions;
