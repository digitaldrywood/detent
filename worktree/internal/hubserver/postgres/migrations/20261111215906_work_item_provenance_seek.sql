-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issues_provenance_lookup_idx;
CREATE INDEX CONCURRENTLY issues_provenance_lookup_idx ON issues (organization_id,project_id,provenance_provider,provenance_external_id,number,id) WHERE provenance_provider IS NOT NULL AND provenance_external_id IS NOT NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY issues_provenance_lookup_idx;
RESET lock_timeout;
