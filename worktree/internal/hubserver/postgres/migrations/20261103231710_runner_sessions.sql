-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_sessions (
    organization_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY,
    project_id text NOT NULL,
    issue_id text NOT NULL,
    attempt_id text NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    start_key text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    backend_id text NOT NULL,
    backend_kind text NOT NULL,
    role text NOT NULL,
    model text NOT NULL,
    policy_id text NOT NULL,
    provider_thread_id text NOT NULL DEFAULT '',
    provider_session_id text NOT NULL DEFAULT '',
    pr_number bigint NOT NULL DEFAULT 0,
    pr_head_sha text NOT NULL DEFAULT '',
    pr_base_sha text NOT NULL DEFAULT '',
    final_state text NOT NULL DEFAULT 'running',
    worker_pid integer NOT NULL DEFAULT 0,
    worker_group_id integer NOT NULL DEFAULT 0,
    worker_started_at timestamptz,
    worker_reaped_at timestamptz,
    payload_object_key text NOT NULL,
    payload_sha256 text NOT NULL,
    payload_bytes bigint NOT NULL,
    payload_content_bytes bigint NOT NULL,
    PRIMARY KEY (organization_id,id),
    UNIQUE(organization_id,project_id,start_key),
    FOREIGN KEY (organization_id,attempt_id) REFERENCES native_attempts(organization_id,id)
);
CREATE INDEX runner_sessions_resume ON runner_sessions(organization_id,project_id,issue_id,started_at DESC,id DESC);
CREATE TABLE runner_session_history (
    organization_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY,
    session_id bigint NOT NULL,
    revision bigint NOT NULL,
    mutation_key text NOT NULL,
    payload_object_key text NOT NULL,
    payload_sha256 text NOT NULL,
    payload_bytes bigint NOT NULL,
    payload_content_bytes bigint NOT NULL,
    PRIMARY KEY(organization_id,session_id,revision),
    UNIQUE(organization_id,id),
    UNIQUE(organization_id,session_id,mutation_key),
    FOREIGN KEY(organization_id,session_id) REFERENCES runner_sessions(organization_id,id)
);
ALTER TABLE runner_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_sessions FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_sessions TO detent_runtime, detent_platform;
GRANT USAGE ON SEQUENCE runner_sessions_id_seq TO detent_runtime, detent_platform, detent_migration;
CREATE POLICY tenant_isolation ON runner_sessions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON runner_sessions TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE runner_session_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_session_history FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_session_history TO detent_runtime, detent_platform;
GRANT USAGE ON SEQUENCE runner_session_history_id_seq TO detent_runtime, detent_platform, detent_migration;
CREATE POLICY tenant_isolation ON runner_session_history TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON runner_session_history TO detent_platform USING (true) WITH CHECK (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runner_session_history;
DROP TABLE runner_sessions;
-- +goose StatementEnd
