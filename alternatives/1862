-- +goose Up
CREATE TABLE hosted_work_views (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 owner_id TEXT NOT NULL REFERENCES hosted_members(user_id) ON DELETE CASCADE,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 scope TEXT NOT NULL CHECK(scope IN ('personal','organization')),
 project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
 layout TEXT NOT NULL CHECK(layout IN ('board','list')),
 tab TEXT NOT NULL CHECK(tab IN ('active','backlog','closed','all')),
 search TEXT NOT NULL,
 sort TEXT NOT NULL,
 completed_window TEXT NOT NULL CHECK(completed_window IN ('1h','24h','7d')),
 archived INTEGER NOT NULL CHECK(archived IN (0,1)),
 created_by TEXT NOT NULL,
 all_lanes INTEGER NOT NULL CHECK(all_lanes IN (0,1)),
 owner_slot INTEGER NOT NULL CHECK(owner_slot >= 0 AND owner_slot < 50),
 organization_slot INTEGER NOT NULL CHECK(organization_slot >= 0 AND organization_slot < 200),
 UNIQUE(organization_id, owner_id, owner_slot),
 UNIQUE(organization_id, organization_slot)
);
CREATE INDEX hosted_work_views_organization ON hosted_work_views(organization_id, owner_id);
CREATE TABLE hosted_work_view_values (
 view_id TEXT NOT NULL REFERENCES hosted_work_views(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('state','type','label','assignee','priority','lanes','collapsed')),
 position INTEGER NOT NULL,
 value TEXT NOT NULL,
 PRIMARY KEY(view_id, kind, position)
);
CREATE TABLE hosted_work_view_favorites (
 view_id TEXT NOT NULL REFERENCES hosted_work_views(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES hosted_members(user_id) ON DELETE CASCADE,
 PRIMARY KEY(view_id, user_id)
);
-- +goose Down
DROP TABLE hosted_work_view_favorites;
DROP TABLE hosted_work_view_values;
DROP TABLE hosted_work_views;
