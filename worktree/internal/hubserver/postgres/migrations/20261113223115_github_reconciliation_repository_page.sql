-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE github_reconciliation
 ADD COLUMN repository_page bigint NOT NULL DEFAULT 0 CHECK (repository_page>=0),
 ADD COLUMN sweep_started_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE github_reconciliation
 DROP COLUMN sweep_started_at,
 DROP COLUMN repository_page;
