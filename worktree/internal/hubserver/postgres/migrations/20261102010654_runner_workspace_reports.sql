-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_workspace_reports (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    terminal boolean NOT NULL,
    files boolean NOT NULL,
    diff boolean NOT NULL,
    preview boolean NOT NULL,
    exec boolean NOT NULL,
    git boolean NOT NULL,
    isolation text NOT NULL DEFAULT '' CHECK (isolation IN ('','sandbox','container','user')),
    PRIMARY KEY (organization_id, runner_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

ALTER TABLE runner_workspace_reports OWNER TO detent_migration;
REVOKE ALL ON runner_workspace_reports FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_workspace_reports TO detent_runtime;
ALTER TABLE runner_workspace_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_workspace_reports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_workspace_reports TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runner_workspace_reports;
-- +goose StatementEnd
