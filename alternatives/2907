-- +goose Up
DROP INDEX search_documents_vector_idx;
CREATE INDEX search_documents_vector_idx ON search_documents USING GIN ((vector || array_to_tsvector(ARRAY['~'||md5(organization_id||':'||md5(id))]))) WITH (fastupdate = off);

-- +goose Down
DROP INDEX search_documents_vector_idx;
CREATE INDEX search_documents_vector_idx ON search_documents USING GIN (vector) WITH (fastupdate = off);
