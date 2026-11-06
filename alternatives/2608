-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_local_checks (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    commit_name text NOT NULL DEFAULT '',
    commit_email text NOT NULL DEFAULT '',
    commit_signing text NOT NULL DEFAULT '',
    checkout_message text NOT NULL DEFAULT '',
    checkout_fix text NOT NULL DEFAULT '',
    runner_setup_declared boolean,
    setup text NOT NULL DEFAULT '',
    checkout text NOT NULL,
    configuration_message text NOT NULL DEFAULT '',
    configuration_fix text NOT NULL DEFAULT '',
    configuration text NOT NULL DEFAULT '',
    checkout_at timestamptz,
    configuration_at timestamptz,
    doctor_at timestamptz,
    provider_at timestamptz,
    doctor text NOT NULL,
    provider text NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE runner_local_check_providers (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    position integer NOT NULL CHECK (position >= 0),
    kind text NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id, position),
    UNIQUE (organization_id, runner_id, project_id, kind),
    FOREIGN KEY (organization_id, runner_id, project_id) REFERENCES runner_local_checks (organization_id, runner_id, project_id) ON DELETE CASCADE
);

ALTER TABLE runner_local_checks OWNER TO detent_migration;
ALTER TABLE runner_local_check_providers OWNER TO detent_migration;
REVOKE ALL ON runner_local_checks, runner_local_check_providers FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_local_checks, runner_local_check_providers TO detent_runtime;
ALTER TABLE runner_local_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_local_checks FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_local_check_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_local_check_providers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_local_checks TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_local_check_providers TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runner_local_check_providers;
DROP TABLE runner_local_checks;
-- +goose StatementEnd
