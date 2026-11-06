-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE native_work_item_page_progress
 ADD COLUMN IF NOT EXISTS backfill_phase smallint NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS backfill_issue_id bigint NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS backfill_comment_id text NOT NULL DEFAULT '';
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_page_dirty_pending;
CREATE INDEX CONCURRENTLY native_work_item_page_dirty_pending ON native_work_item_page_dirty(organization_id,project_id,work_item_id,kind) WHERE NOT cleaning;
-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_page_dirty_pending;
ALTER TABLE native_work_item_page_progress DROP COLUMN IF EXISTS backfill_phase,DROP COLUMN IF EXISTS backfill_issue_id,DROP COLUMN IF EXISTS backfill_comment_id;
