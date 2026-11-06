-- +goose Up
CREATE TABLE hosted_work_view_preferences (
    organization_id text NOT NULL,
    user_id text NOT NULL,
    scope text NOT NULL,
    view_query text NOT NULL CHECK (octet_length(view_query) <= 8192),
    PRIMARY KEY (organization_id, user_id, scope),
    FOREIGN KEY (organization_id, user_id) REFERENCES hosted_members (organization_id, user_id) ON DELETE CASCADE
);

CREATE TABLE hosted_work_views (
    organization_id text NOT NULL,
    id text NOT NULL,
    owner_id text NOT NULL,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    scope text NOT NULL CHECK (scope IN ('personal', 'organization')),
    project_id text,
    layout text NOT NULL CHECK (layout IN ('board', 'list')),
    tab text NOT NULL CHECK (tab IN ('active', 'backlog', 'closed', 'all')),
    search text NOT NULL,
    sort text NOT NULL,
    completed_window text NOT NULL CHECK (completed_window IN ('1h', '24h', '7d')),
    completed_from text NOT NULL,
    completed_to text NOT NULL,
    archived boolean NOT NULL,
    created_by text NOT NULL,
    all_lanes boolean NOT NULL,
    owner_slot integer NOT NULL CHECK (owner_slot BETWEEN 0 AND 49),
    organization_slot integer NOT NULL CHECK (organization_slot BETWEEN 0 AND 199),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, owner_id, owner_slot),
    UNIQUE (organization_id, organization_slot),
    FOREIGN KEY (organization_id, owner_id) REFERENCES hosted_members (organization_id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE hosted_work_view_values (
    organization_id text NOT NULL,
    view_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('state', 'type', 'label', 'assignee', 'priority', 'lanes', 'collapsed')),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 255),
    value text NOT NULL,
    PRIMARY KEY (organization_id, view_id, kind, position),
    FOREIGN KEY (organization_id, view_id) REFERENCES hosted_work_views (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE hosted_work_view_projects (
    organization_id text NOT NULL,
    view_id text NOT NULL,
    project_id text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 99),
    PRIMARY KEY (organization_id, view_id, project_id),
    UNIQUE (organization_id, view_id, position),
    FOREIGN KEY (organization_id, view_id) REFERENCES hosted_work_views (organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE hosted_work_view_favorites (
    organization_id text NOT NULL,
    view_id text NOT NULL,
    user_id text NOT NULL,
    PRIMARY KEY (organization_id, view_id, user_id),
    FOREIGN KEY (organization_id, view_id) REFERENCES hosted_work_views (organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, user_id) REFERENCES hosted_members (organization_id, user_id) ON DELETE CASCADE
);

ALTER TABLE hosted_work_view_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_work_view_preferences FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_work_view_preferences FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_work_view_preferences TO detent_runtime;
CREATE POLICY tenant_isolation ON hosted_work_view_preferences TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_work_views ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_work_views FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_work_views FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_work_views TO detent_runtime;
CREATE POLICY tenant_isolation ON hosted_work_views TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_work_view_values ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_work_view_values FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_work_view_values FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_work_view_values TO detent_runtime;
CREATE POLICY tenant_isolation ON hosted_work_view_values TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_work_view_projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_work_view_projects FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_work_view_projects FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_work_view_projects TO detent_runtime;
CREATE POLICY tenant_isolation ON hosted_work_view_projects TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_work_view_favorites ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_work_view_favorites FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_work_view_favorites FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_work_view_favorites TO detent_runtime;
CREATE POLICY tenant_isolation ON hosted_work_view_favorites TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE hosted_work_view_favorites;
DROP TABLE hosted_work_view_projects;
DROP TABLE hosted_work_view_values;
DROP TABLE hosted_work_views;
DROP TABLE hosted_work_view_preferences;
