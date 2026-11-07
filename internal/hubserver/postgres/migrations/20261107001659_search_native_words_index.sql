-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS search_document_index_words_gin;
CREATE INDEX CONCURRENTLY search_document_index_words_gin ON search_document_index USING gin (word_vector);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS search_document_index_words_gin;
