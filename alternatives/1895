-- +goose Up
ALTER TABLE project_sprite_pools ADD COLUMN isolation_tier TEXT NOT NULL DEFAULT 'native-trusted' CHECK (isolation_tier IN ('sandbox', 'native-trusted'));

-- +goose Down
ALTER TABLE project_sprite_pools DROP COLUMN isolation_tier;
