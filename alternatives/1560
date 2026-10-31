-- +goose Up
ALTER TABLE github_outbox ADD COLUMN coalesce_key TEXT;
ALTER TABLE github_outbox ADD COLUMN processing_started_at TEXT;
ALTER TABLE github_outbox ADD COLUMN operator_action TEXT;

CREATE INDEX github_outbox_coalesce_idx
ON github_outbox(coalesce_key, status, id);

CREATE INDEX github_outbox_processing_idx
ON github_outbox(status, processing_started_at, id);
