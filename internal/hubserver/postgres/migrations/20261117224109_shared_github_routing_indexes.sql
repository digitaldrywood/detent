-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_entry_repository;
CREATE INDEX CONCURRENTLY github_entry_repository ON project_settings (lower(repository),organization_id,project_id) WHERE repository <> '';
DROP INDEX CONCURRENTLY IF EXISTS github_entry_checkout_repository;
CREATE INDEX CONCURRENTLY github_entry_checkout_repository ON project_settings (lower(checkout_repository),organization_id,project_id) WHERE checkout_repository <> '';
DROP INDEX CONCURRENTLY IF EXISTS github_entry_receipt;
CREATE INDEX CONCURRENTLY github_entry_receipt ON github_webhook_receipts ((COALESCE(last_received_at,received_at)) DESC,organization_id);
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS github_entry_receipt;
DROP INDEX CONCURRENTLY IF EXISTS github_entry_checkout_repository;
DROP INDEX CONCURRENTLY IF EXISTS github_entry_repository;
