-- +goose Up
ALTER TABLE project_sprite_allocations ADD COLUMN provision_failures INTEGER NOT NULL DEFAULT 0 CHECK (provision_failures >= 0);
ALTER TABLE organization_sprite_members ADD COLUMN allocation_revision INTEGER NOT NULL DEFAULT 0 CHECK (allocation_revision >= 0);
