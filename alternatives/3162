-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_webhook_latest_receipt;
CREATE INDEX CONCURRENTLY github_webhook_latest_receipt ON github_webhook_receipts (organization_id,(COALESCE(last_received_at,received_at)) DESC,delivery_id DESC);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_webhook_latest_receipt;
RESET lock_timeout;
