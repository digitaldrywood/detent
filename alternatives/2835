-- +goose Up
ALTER TABLE backlog_admission_runs ADD COLUMN candidate_progress_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE backlog_admission_runs DROP COLUMN candidate_progress_json;
