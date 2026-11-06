-- +goose Up
ALTER TABLE backlog_admission_runs ADD COLUMN resume_at TEXT;

-- +goose Down
ALTER TABLE backlog_admission_runs DROP COLUMN resume_at;
