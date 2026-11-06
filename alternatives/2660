-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_board_items_work_order ON native_board_items
 (organization_id, project_id, running DESC, dispatchable DESC, issue_number DESC, identifier);
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_board_items_number_pending ON native_board_items
 (organization_id, project_id) WHERE issue_number IS NULL;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace
  AND c.relname IN ('native_board_items_work_order','native_board_items_number_pending')
  AND (NOT i.indisvalid OR NOT i.indisready)
 ) THEN RAISE EXCEPTION 'Board work order indexes require repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_items_number_pending;
DROP INDEX CONCURRENTLY IF EXISTS native_board_items_work_order;
RESET lock_timeout;
