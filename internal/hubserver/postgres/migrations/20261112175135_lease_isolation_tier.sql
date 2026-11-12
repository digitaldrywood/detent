-- +goose Up
ALTER TABLE leases ADD COLUMN isolation_tier text CHECK (isolation_tier IN ('sandbox','native-trusted'));

-- +goose Down
ALTER TABLE leases DROP COLUMN isolation_tier;
