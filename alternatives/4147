-- +goose Up
ALTER TABLE validator_verdicts ADD COLUMN failure_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE validator_verdicts ADD COLUMN next_retry_at TEXT;

-- +goose Down
ALTER TABLE validator_verdicts DROP COLUMN next_retry_at;
ALTER TABLE validator_verdicts DROP COLUMN failure_attempts;
