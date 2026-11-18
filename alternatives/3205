-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issues_storage_archive_candidates;
CREATE INDEX CONCURRENTLY issues_storage_archive_candidates ON issues(organization_id,terminal_entered_at,id) WHERE terminal_entered_at IS NOT NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY issues_storage_archive_candidates;
RESET lock_timeout;
