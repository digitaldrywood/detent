-- +goose Up
ALTER TABLE native_progress_evidence ADD COLUMN retrieved INTEGER NOT NULL DEFAULT 0 CHECK (retrieved IN (0, 1));
ALTER TABLE native_progress_evidence ADD COLUMN retrieved_attempt_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_progress_evidence ADD COLUMN retrieved_source_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE native_progress_evidence DROP COLUMN retrieved_source_sha;
ALTER TABLE native_progress_evidence DROP COLUMN retrieved_attempt_id;
ALTER TABLE native_progress_evidence DROP COLUMN retrieved;
