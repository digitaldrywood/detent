-- +goose Up
ALTER TABLE runner_identities
    ADD COLUMN state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','draining','disabled')),
    ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    ADD COLUMN tags text[] NOT NULL DEFAULT '{}',
    ADD COLUMN machine_use text NOT NULL DEFAULT 'shared' CHECK (machine_use IN ('shared','dedicated')),
    ADD COLUMN isolation_tier text NOT NULL DEFAULT 'sandbox' CHECK (isolation_tier IN ('sandbox','native-trusted')),
    ADD COLUMN host_services text[] NOT NULL DEFAULT '{}',
    ADD COLUMN availability_timezone text NOT NULL DEFAULT '',
    ADD COLUMN availability_hard_deadline text NOT NULL DEFAULT '';

CREATE TABLE runner_availability_windows (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 63),
    window_spec text NOT NULL,
    PRIMARY KEY (organization_id, runner_id, position),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

CREATE TABLE runner_availability_window_slots (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    slot_key text NOT NULL,
    capacity integer NOT NULL CHECK (capacity >= 0 AND capacity <= 10000),
    PRIMARY KEY (organization_id, runner_id, slot_key),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

CREATE TABLE runner_project_capacity_limits (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    capacity_limit integer NOT NULL CHECK (capacity_limit >= 0 AND capacity_limit <= 10000),
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE runner_project_ranks (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    rank integer NOT NULL CHECK (rank >= 0),
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- +goose StatementBegin
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['runner_availability_windows','runner_availability_window_slots','runner_project_capacity_limits','runner_project_ranks'] LOOP
        EXECUTE format('ALTER TABLE %I OWNER TO detent_migration', table_name);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC, detent_platform', table_name);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO detent_runtime', table_name);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting(''detent.organization_id'', true), '''')) WITH CHECK (organization_id = NULLIF(current_setting(''detent.organization_id'', true), ''''))', table_name);
    END LOOP;
END $$;
-- +goose StatementEnd

ALTER TABLE runner_identities OWNER TO detent_migration;

-- +goose Down
DROP TABLE runner_project_ranks;
DROP TABLE runner_project_capacity_limits;
DROP TABLE runner_availability_window_slots;
DROP TABLE runner_availability_windows;
ALTER TABLE runner_identities
    DROP COLUMN availability_hard_deadline,
    DROP COLUMN availability_timezone,
    DROP COLUMN host_services,
    DROP COLUMN isolation_tier,
    DROP COLUMN machine_use,
    DROP COLUMN tags,
    DROP COLUMN revision,
    DROP COLUMN state;
