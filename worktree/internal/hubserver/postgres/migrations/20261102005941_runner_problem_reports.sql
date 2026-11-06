-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_problem_reports (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    code text NOT NULL CHECK (code IN ('tier_unavailable','backend_missing','backend_probe_failed','host_service_unreachable','settings_invalid','keep_awake_failed','settings_rejected','version_unsupported','policy_mismatch','update_applied_not_running')),
    project_id text NOT NULL DEFAULT '',
    subject text NOT NULL DEFAULT '',
    check_name text NOT NULL DEFAULT '',
    error_output text NOT NULL DEFAULT '',
    fix_command text NOT NULL DEFAULT '',
    message text NOT NULL,
    fix_hint text NOT NULL,
    reported_at timestamptz NOT NULL,
    first_seen timestamptz NOT NULL,
    PRIMARY KEY (organization_id, runner_id, code, project_id, subject, check_name),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

ALTER TABLE runner_problem_reports OWNER TO detent_migration;
REVOKE ALL ON runner_problem_reports FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_problem_reports TO detent_runtime;
ALTER TABLE runner_problem_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_problem_reports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_problem_reports TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runner_problem_reports;
-- +goose StatementEnd
