-- +goose Up
ALTER TABLE issues ADD COLUMN verification_state TEXT CHECK (verification_state IN ('pending', 'verified'));
ALTER TABLE issues ADD COLUMN verified_by_json TEXT CHECK (verified_by_json IS NULL OR json_valid(verified_by_json));
ALTER TABLE issues ADD COLUMN verified_at TEXT;

-- +goose Down
ALTER TABLE issues DROP COLUMN verified_at;
ALTER TABLE issues DROP COLUMN verified_by_json;
ALTER TABLE issues DROP COLUMN verification_state;
