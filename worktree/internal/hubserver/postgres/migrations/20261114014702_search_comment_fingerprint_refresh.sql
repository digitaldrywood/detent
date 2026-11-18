-- +goose Up
CREATE POLICY search_comment_fingerprint_migration ON search_documents TO detent_migration USING(true) WITH CHECK(true);
UPDATE search_documents SET indexed_revision=0,refresh_order=nextval('search_refresh_order')
WHERE kind='comment' AND NOT placeholder AND indexed_revision=revision AND terms && ARRAY['go','fail','goos','duplic','detent']::text[];
DROP POLICY search_comment_fingerprint_migration ON search_documents;

-- +goose Down
SELECT 1;
