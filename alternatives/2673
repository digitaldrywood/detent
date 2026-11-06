-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_board_workspace_pending ON native_board_items
 (organization_id, project_id) WHERE NOT workspace_counted;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('public.native_board_workspace_pending') AND indisvalid AND indisready)
 THEN RAISE EXCEPTION 'Board workspace pending index requires repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_workspace_pending;
RESET lock_timeout;
