-- +goose Up
ALTER TABLE search_postings ADD COLUMN literal_positions bigint[] NOT NULL DEFAULT '{}';
UPDATE search_documents SET indexed_revision = 0 WHERE kind = 'issue';

-- +goose Down
ALTER TABLE search_postings DROP COLUMN literal_positions;
