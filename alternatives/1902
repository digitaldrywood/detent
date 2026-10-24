-- +goose Up
ALTER TABLE organization_sprite_pools ADD COLUMN share_idle_capacity INTEGER NOT NULL DEFAULT 0 CHECK (share_idle_capacity IN (0, 1));
