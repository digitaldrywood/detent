-- +goose Up
ALTER TABLE project_sprite_allocations ADD COLUMN provision_failures INTEGER NOT NULL DEFAULT 0 CHECK (provision_failures >= 0);
