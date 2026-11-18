-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS attempt_diffs_unindexed_bodies_idx ON attempt_diffs
 (organization_id, project_id) WHERE NOT body_references_indexed AND NOT manifest_expired;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace AND c.relname='attempt_diffs_unindexed_bodies_idx'
  AND (NOT i.indisvalid OR NOT i.indisready)
 ) THEN RAISE EXCEPTION 'Attempt diff reference index requires repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS attempt_diffs_unindexed_bodies_idx;
RESET lock_timeout;
