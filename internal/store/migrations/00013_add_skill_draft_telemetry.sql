-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN skill_draft_proposed INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE codex_sessions
DROP COLUMN skill_draft_proposed;
