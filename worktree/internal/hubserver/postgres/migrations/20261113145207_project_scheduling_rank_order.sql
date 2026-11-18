-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS projects_scheduling_rank_order ON projects
 (organization_id, scheduling_rank, id) WHERE deleted_at IS NULL;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace
  AND c.relname='projects_scheduling_rank_order'
  AND (NOT i.indisvalid OR NOT i.indisready)
 ) THEN RAISE EXCEPTION 'Project scheduling rank index requires repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS projects_scheduling_rank_order;
RESET lock_timeout;
