-- +goose Up
ALTER TABLE runner_project_observations
    ADD COLUMN diagnostics_source text NOT NULL DEFAULT '',
    ADD COLUMN diagnostics_truncated boolean NOT NULL DEFAULT false,
    ADD COLUMN build_version text,
    ADD COLUMN build_commit text,
    ADD COLUMN build_source text,
    ADD COLUMN build_sha256 text,
    ADD COLUMN build_os text,
    ADD COLUMN build_architecture text,
    ADD COLUMN build_verified_release boolean,
    ADD COLUMN build_observed_at timestamptz;

ALTER TABLE runner_project_diagnostic_attempts
    ADD COLUMN membership text[] NOT NULL DEFAULT '{}',
    ADD COLUMN operation_started_at timestamptz,
    ADD COLUMN operation_observed_at timestamptz,
    ADD COLUMN latest_error_code text NOT NULL DEFAULT '',
    ADD COLUMN latest_error text NOT NULL DEFAULT '',
    ADD COLUMN deferred_at timestamptz,
    ADD COLUMN retry_at timestamptz,
    ADD COLUMN retry_attempt integer NOT NULL DEFAULT 0 CHECK (retry_attempt >= 0),
    ADD COLUMN recovery_owner text NOT NULL DEFAULT '',
    ADD COLUMN lease_expires_at timestamptz;

CREATE TABLE runner_project_diagnostic_counts (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    count_key text NOT NULL,
    count_value integer NOT NULL CHECK (count_value >= 0),
    PRIMARY KEY (organization_id, runner_id, project_id, count_key),
    FOREIGN KEY (organization_id, runner_id, project_id) REFERENCES runner_project_observations (organization_id, runner_id, project_id) ON DELETE CASCADE
);

CREATE TABLE runner_project_diagnostic_unavailable (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    source text NOT NULL CHECK (source IN ('snapshot', 'attempt', 'admission')),
    position integer NOT NULL DEFAULT -1,
    unavailable_key text NOT NULL,
    reason text NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id, source, position, unavailable_key),
    FOREIGN KEY (organization_id, runner_id, project_id) REFERENCES runner_project_observations (organization_id, runner_id, project_id) ON DELETE CASCADE
);

CREATE TABLE runner_project_diagnostic_admissions (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 255),
    issue_id text NOT NULL,
    work_item_revision bigint NOT NULL DEFAULT 0,
    observed_at timestamptz NOT NULL,
    result text NOT NULL,
    predicate text NOT NULL,
    repository_reservations text[] NOT NULL DEFAULT '{}',
    host_landing_occupancy integer NOT NULL DEFAULT 0,
    provider_role text NOT NULL DEFAULT '',
    provider_backend text NOT NULL DEFAULT '',
    provider_model text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id, runner_id, project_id, position),
    FOREIGN KEY (organization_id, runner_id, project_id) REFERENCES runner_project_observations (organization_id, runner_id, project_id) ON DELETE CASCADE
);

CREATE INDEX runner_project_observation_project_idx ON runner_project_observations (organization_id, project_id, runner_id);

ALTER TABLE runner_project_diagnostic_counts OWNER TO detent_migration;
ALTER TABLE runner_project_diagnostic_unavailable OWNER TO detent_migration;
ALTER TABLE runner_project_diagnostic_admissions OWNER TO detent_migration;
REVOKE ALL ON runner_project_diagnostic_counts, runner_project_diagnostic_unavailable, runner_project_diagnostic_admissions FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_project_diagnostic_counts, runner_project_diagnostic_unavailable, runner_project_diagnostic_admissions TO detent_runtime;
ALTER TABLE runner_project_diagnostic_counts ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_counts FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_unavailable ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_unavailable FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_admissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_admissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_project_diagnostic_counts TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_project_diagnostic_unavailable TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_project_diagnostic_admissions TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP INDEX runner_project_observation_project_idx;
DROP TABLE runner_project_diagnostic_admissions;
DROP TABLE runner_project_diagnostic_unavailable;
DROP TABLE runner_project_diagnostic_counts;
ALTER TABLE runner_project_diagnostic_attempts
    DROP COLUMN membership,
    DROP COLUMN operation_started_at,
    DROP COLUMN operation_observed_at,
    DROP COLUMN latest_error_code,
    DROP COLUMN latest_error,
    DROP COLUMN deferred_at,
    DROP COLUMN retry_at,
    DROP COLUMN retry_attempt,
    DROP COLUMN recovery_owner,
    DROP COLUMN lease_expires_at;
ALTER TABLE runner_project_observations
    DROP COLUMN diagnostics_source,
    DROP COLUMN diagnostics_truncated,
    DROP COLUMN build_version,
    DROP COLUMN build_commit,
    DROP COLUMN build_source,
    DROP COLUMN build_sha256,
    DROP COLUMN build_os,
    DROP COLUMN build_architecture,
    DROP COLUMN build_verified_release,
    DROP COLUMN build_observed_at;
