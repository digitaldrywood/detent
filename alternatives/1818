-- +goose Up
ALTER TABLE issues ADD COLUMN blocked_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN blocked_reason_basis TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN blocked_reason_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN unblocks_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN unblocks_reason_basis TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN unblocks_reason_revision INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE issues DROP COLUMN unblocks_reason_revision;
ALTER TABLE issues DROP COLUMN unblocks_reason_basis;
ALTER TABLE issues DROP COLUMN unblocks_reason;
ALTER TABLE issues DROP COLUMN blocked_reason_revision;
ALTER TABLE issues DROP COLUMN blocked_reason_basis;
ALTER TABLE issues DROP COLUMN blocked_reason;
