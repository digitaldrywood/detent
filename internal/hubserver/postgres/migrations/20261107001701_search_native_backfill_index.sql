-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS search_documents_native_backfill_idx;
CREATE INDEX CONCURRENTLY search_documents_native_backfill_idx ON search_documents (organization_id, id COLLATE "C")
 WHERE search_format=0 AND kind IN ('issue','comment','thread');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS search_documents_native_backfill_idx;
