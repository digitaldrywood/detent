-- +goose Up
ALTER TABLE leases ADD COLUMN landing_capacity INTEGER NOT NULL DEFAULT 0 CHECK (landing_capacity >= 0);

-- +goose Down
ALTER TABLE leases DROP COLUMN landing_capacity;
