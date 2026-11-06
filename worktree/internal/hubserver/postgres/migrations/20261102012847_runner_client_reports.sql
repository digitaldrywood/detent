-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_project_skills (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    position integer NOT NULL CHECK (position >= 0),
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    path text NOT NULL,
    scope text NOT NULL CHECK (scope IN ('project', 'personal')),
    enabled boolean NOT NULL,
    user_invocable boolean NOT NULL,
    invocation text NOT NULL,
    reported_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id, position),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX runner_project_skills_project_idx ON runner_project_skills (organization_id, project_id, reported_at DESC, runner_id, position);

CREATE TABLE runner_checkout_repositories (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    repository text NOT NULL,
    reported_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX runner_checkout_repositories_verified_idx ON runner_checkout_repositories (organization_id, project_id, lower(repository), reported_at DESC, runner_id);

ALTER TABLE runner_project_skills OWNER TO detent_migration;
ALTER TABLE runner_checkout_repositories OWNER TO detent_migration;
REVOKE ALL ON runner_project_skills, runner_checkout_repositories FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_project_skills, runner_checkout_repositories TO detent_runtime;
ALTER TABLE runner_project_skills ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_skills FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_checkout_repositories ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_checkout_repositories FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_project_skills TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_checkout_repositories TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runner_checkout_repositories;
DROP TABLE runner_project_skills;
-- +goose StatementEnd
