-- +goose Up
CREATE TABLE runner_provider_reports (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    backend text NOT NULL,
    provider text NOT NULL,
    account_alias text NOT NULL,
    shared_account_alias text NOT NULL DEFAULT '',
    billing text NOT NULL CHECK (billing IN ('subscription','api_key','unknown')),
    max_concurrent integer NOT NULL CHECK (max_concurrent BETWEEN 0 AND 10000),
    availability text NOT NULL CHECK (availability IN ('unknown','available','exhausted')),
    observed_at timestamptz NOT NULL,
    reset_at timestamptz,
    PRIMARY KEY (organization_id, runner_id, backend),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

CREATE TABLE runner_provider_report_models (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    backend text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 127),
    model_id text NOT NULL,
    has_detail boolean NOT NULL,
    label text NOT NULL DEFAULT '',
    model_provider text NOT NULL DEFAULT '',
    is_default boolean NOT NULL,
    default_reasoning_effort text NOT NULL DEFAULT '',
    legacy boolean NOT NULL,
    PRIMARY KEY (organization_id, runner_id, backend, position),
    UNIQUE (organization_id, runner_id, backend, model_id),
    FOREIGN KEY (organization_id, runner_id, backend) REFERENCES runner_provider_reports (organization_id, runner_id, backend) ON DELETE CASCADE
);

CREATE TABLE runner_provider_model_efforts (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    backend text NOT NULL,
    model_id text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 15),
    effort text NOT NULL,
    PRIMARY KEY (organization_id, runner_id, backend, model_id, position),
    UNIQUE (organization_id, runner_id, backend, model_id, effort),
    FOREIGN KEY (organization_id, runner_id, backend, model_id) REFERENCES runner_provider_report_models (organization_id, runner_id, backend, model_id) ON DELETE CASCADE
);

CREATE TABLE runner_provider_usage_windows (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    backend text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 7),
    kind text NOT NULL,
    percent_used double precision,
    reset_at timestamptz,
    observed_at timestamptz,
    PRIMARY KEY (organization_id, runner_id, backend, position),
    FOREIGN KEY (organization_id, runner_id, backend) REFERENCES runner_provider_reports (organization_id, runner_id, backend) ON DELETE CASCADE
);

-- +goose StatementBegin
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['runner_provider_reports','runner_provider_report_models','runner_provider_model_efforts','runner_provider_usage_windows'] LOOP
        EXECUTE format('ALTER TABLE %I OWNER TO detent_migration', table_name);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC, detent_platform', table_name);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO detent_runtime', table_name);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting(''detent.organization_id'', true), '''')) WITH CHECK (organization_id = NULLIF(current_setting(''detent.organization_id'', true), ''''))', table_name);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE runner_provider_usage_windows;
DROP TABLE runner_provider_model_efforts;
DROP TABLE runner_provider_report_models;
DROP TABLE runner_provider_reports;
