-- +goose Up
CREATE TABLE machines (
    organization_id text NOT NULL,
    id text NOT NULL CHECK (btrim(id) <> ''),
    hostname text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    capacity bigint NOT NULL CHECK (capacity >= 0),
    version text NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    registered_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE leases (
    organization_id text NOT NULL,
    lease_id text NOT NULL CHECK (btrim(lease_id) <> ''),
    fencing_token bigint GENERATED ALWAYS AS IDENTITY NOT NULL CHECK (fencing_token > 0),
    issue_id bigint NOT NULL,
    machine_id text NOT NULL,
    session_id text NOT NULL CHECK (btrim(session_id) <> ''),
    work_item_revision bigint NOT NULL CHECK (work_item_revision > 0),
    landing_capacity bigint NOT NULL DEFAULT 0 CHECK (landing_capacity >= 0),
    acquired_at timestamptz NOT NULL,
    renewed_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    released_at timestamptz,
    release_reason text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id, lease_id),
    UNIQUE (organization_id, fencing_token),
    UNIQUE (organization_id, session_id),
    UNIQUE (organization_id, lease_id, fencing_token, issue_id, work_item_revision),
    CHECK (renewed_at >= acquired_at AND expires_at > renewed_at),
    CHECK (released_at IS NULL OR released_at >= acquired_at),
    FOREIGN KEY (organization_id, issue_id) REFERENCES issues (organization_id, id),
    FOREIGN KEY (organization_id, machine_id) REFERENCES machines (organization_id, id)
);

CREATE UNIQUE INDEX leases_active_issue_idx ON leases (organization_id, issue_id) WHERE released_at IS NULL;
CREATE INDEX leases_expiry_idx ON leases (organization_id, expires_at, lease_id) WHERE released_at IS NULL;
CREATE INDEX leases_machine_usage_idx ON leases (organization_id, machine_id, expires_at) WHERE released_at IS NULL;

CREATE TABLE native_attempt_runs (
    organization_id text NOT NULL,
    run_id text NOT NULL CHECK (btrim(run_id) <> ''),
    issue_id bigint NOT NULL,
    PRIMARY KEY (organization_id, run_id),
    UNIQUE (organization_id, run_id, issue_id),
    FOREIGN KEY (organization_id, issue_id) REFERENCES issues (organization_id, id)
);

CREATE TABLE native_attempts (
    organization_id text NOT NULL,
    id text NOT NULL CHECK (btrim(id) <> ''),
    issue_id bigint NOT NULL,
    lease_id text NOT NULL,
    fencing_token bigint NOT NULL CHECK (fencing_token > 0),
    run_id text NOT NULL CHECK (btrim(run_id) <> ''),
    sequence bigint NOT NULL CHECK (sequence > 0),
    status text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled', 'interrupted')),
    work_item_revision bigint NOT NULL CHECK (work_item_revision > 0),
    dispatch_generation bigint NOT NULL CHECK (dispatch_generation >= 0),
    payload_object_key text NOT NULL CHECK (btrim(payload_object_key) <> ''),
    checkpoint_object_key text,
    artifacts_object_key text,
    started_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, lease_id),
    UNIQUE (organization_id, fencing_token),
    FOREIGN KEY (organization_id, run_id, issue_id) REFERENCES native_attempt_runs (organization_id, run_id, issue_id),
    FOREIGN KEY (organization_id, lease_id, fencing_token, issue_id, work_item_revision)
        REFERENCES leases (organization_id, lease_id, fencing_token, issue_id, work_item_revision)
);

CREATE INDEX native_attempts_item_idx ON native_attempts (organization_id, issue_id, fencing_token);

CREATE TABLE native_attempt_events (
    organization_id text NOT NULL,
    attempt_id text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (organization_id, attempt_id, sequence),
    FOREIGN KEY (organization_id, attempt_id) REFERENCES native_attempts (organization_id, id)
);

ALTER TABLE machines ENABLE ROW LEVEL SECURITY;
ALTER TABLE machines FORCE ROW LEVEL SECURITY;
REVOKE ALL ON machines FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON machines TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON machines TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON machines TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE leases FORCE ROW LEVEL SECURITY;
REVOKE ALL ON leases FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON leases TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON leases TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON leases TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE native_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_attempts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_attempts FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_attempts TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_attempts TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_attempts TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE native_attempt_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_attempt_events FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_attempt_events FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_attempt_events TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_attempt_events TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_attempt_events TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE native_attempt_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_attempt_runs FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_attempt_runs FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_attempt_runs TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_attempt_runs TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_attempt_runs TO detent_platform USING (true) WITH CHECK (true);

GRANT USAGE ON SEQUENCE leases_fencing_token_seq TO detent_runtime, detent_platform;

-- +goose Down
DROP TABLE native_attempt_events;
DROP TABLE native_attempts;
DROP TABLE native_attempt_runs;
DROP TABLE leases;
DROP TABLE machines;
