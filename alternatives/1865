-- +goose Up
CREATE TABLE project_sprite_allocations (
 organization_id TEXT NOT NULL REFERENCES organization_sprite_pools(organization_id) ON DELETE CASCADE,
 project_id TEXT NOT NULL,
 desired_count INTEGER NOT NULL CHECK (desired_count >= 0),
 revision INTEGER NOT NULL CHECK (revision > 0),
 requested_at TEXT NOT NULL,
 configured_by TEXT NOT NULL REFERENCES api_tokens(id),
 PRIMARY KEY (organization_id, project_id),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);

ALTER TABLE organization_sprite_members ADD COLUMN allocation_project_id TEXT NOT NULL DEFAULT '';
CREATE INDEX organization_sprite_members_allocation ON organization_sprite_members(organization_id, allocation_project_id, state);
