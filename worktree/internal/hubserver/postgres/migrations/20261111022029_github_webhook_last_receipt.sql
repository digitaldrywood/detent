-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_webhook_receipts ADD COLUMN last_received_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_webhook_receipts DROP COLUMN last_received_at;
