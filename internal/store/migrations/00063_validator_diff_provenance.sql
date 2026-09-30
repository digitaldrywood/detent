-- +goose Up
ALTER TABLE validator_verdicts ADD COLUMN repository TEXT NOT NULL DEFAULT '';
ALTER TABLE validator_verdicts ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE validator_verdicts ADD COLUMN diff_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE validator_verdicts ADD COLUMN diff_files_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE validator_verdicts DROP COLUMN diff_files_json;
ALTER TABLE validator_verdicts DROP COLUMN diff_digest;
ALTER TABLE validator_verdicts DROP COLUMN base_sha;
ALTER TABLE validator_verdicts DROP COLUMN repository;
