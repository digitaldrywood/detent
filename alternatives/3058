-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS search_document_index_text_gin;
CREATE INDEX CONCURRENTLY search_document_index_text_gin ON search_document_index USING gin (search_text gin_trgm_ops);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS search_document_index_text_gin;
