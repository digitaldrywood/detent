-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_attempt_token_usage (
    organization_id text NOT NULL,
    attempt_id text NOT NULL,
    model text NOT NULL,
    project_id text NOT NULL,
    issue_id bigint NOT NULL,
    usage_day date NOT NULL,
    observed_at timestamptz NOT NULL,
    input_tokens bigint NOT NULL CHECK (input_tokens >= 0),
    cached_input_tokens bigint NOT NULL CHECK (cached_input_tokens >= 0),
    output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
    PRIMARY KEY (organization_id, attempt_id, model),
    FOREIGN KEY (organization_id, attempt_id) REFERENCES native_attempts(organization_id, id)
);
CREATE INDEX runner_token_usage_day ON runner_attempt_token_usage(organization_id, project_id, usage_day);
CREATE INDEX runner_token_usage_issue ON runner_attempt_token_usage(organization_id, project_id, issue_id);
CREATE INDEX runner_token_usage_model ON runner_attempt_token_usage(organization_id, project_id, model, observed_at DESC, attempt_id);
ALTER TABLE runner_attempt_token_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_attempt_token_usage FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_attempt_token_usage TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON runner_attempt_token_usage TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON runner_attempt_token_usage TO detent_platform USING (true) WITH CHECK (true);
CREATE TABLE runner_project_daily_tokens (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    usage_day date NOT NULL,
    model text NOT NULL,
    input_tokens bigint NOT NULL,
    cached_input_tokens bigint NOT NULL,
    output_tokens bigint NOT NULL,
    sessions bigint NOT NULL,
    PRIMARY KEY(organization_id,project_id,usage_day,model),
    FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE TABLE runner_issue_tokens (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    issue_id bigint NOT NULL,
    model text NOT NULL,
    input_tokens bigint NOT NULL,
    cached_input_tokens bigint NOT NULL,
    output_tokens bigint NOT NULL,
    sessions bigint NOT NULL,
    PRIMARY KEY(organization_id,project_id,issue_id,model),
    FOREIGN KEY(organization_id,issue_id) REFERENCES issues(organization_id,id)
);
ALTER TABLE runner_project_daily_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_daily_tokens FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_project_daily_tokens TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON runner_project_daily_tokens TO detent_runtime,detent_migration
    USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON runner_project_daily_tokens TO detent_platform USING(true) WITH CHECK(true);
ALTER TABLE runner_issue_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_issue_tokens FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_issue_tokens TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON runner_issue_tokens TO detent_runtime,detent_migration
    USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON runner_issue_tokens TO detent_platform USING(true) WITH CHECK(true);
ALTER TABLE native_attempts ADD COLUMN runner_tokens_recorded boolean NOT NULL DEFAULT false;
CREATE INDEX runner_unaccounted_tokens ON native_attempts(organization_id,issue_id) WHERE NOT runner_tokens_recorded;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE native_attempts DROP COLUMN runner_tokens_recorded;
DROP TABLE runner_issue_tokens;
DROP TABLE runner_project_daily_tokens;
DROP TABLE runner_attempt_token_usage;
-- +goose StatementEnd
