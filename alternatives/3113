-- +goose Up
ALTER TABLE lane_ledger ADD COLUMN origin TEXT NOT NULL DEFAULT '';
ALTER TABLE lane_ledger ADD COLUMN action_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE lane_ledger ADD COLUMN resolved_at TEXT;

-- +goose Down
ALTER TABLE lane_ledger DROP COLUMN resolved_at;
ALTER TABLE lane_ledger DROP COLUMN action_kind;
ALTER TABLE lane_ledger DROP COLUMN origin;
