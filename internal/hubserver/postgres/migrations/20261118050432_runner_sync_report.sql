-- +goose Up
ALTER TABLE runner_identities ADD COLUMN sync_report_digest text NOT NULL DEFAULT ''
 CHECK (sync_report_digest = '' OR sync_report_digest ~ '^[0-9a-f]{64}$');

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN sync_report_digest;
