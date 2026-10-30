-- +goose Up
ALTER TABLE issues ADD COLUMN progress_evidence_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN progress_human_comment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN progress_comment_sequence INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE issues DROP COLUMN progress_comment_sequence;
ALTER TABLE issues DROP COLUMN progress_human_comment_id;
ALTER TABLE issues DROP COLUMN progress_evidence_revision;
