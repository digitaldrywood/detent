-- +goose Up
ALTER TABLE efficiency_receipts ADD COLUMN in_progress INTEGER NOT NULL DEFAULT 0;
ALTER TABLE efficiency_receipts ADD COLUMN refreshed_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE efficiency_receipts DROP COLUMN refreshed_at;
ALTER TABLE efficiency_receipts DROP COLUMN in_progress;
