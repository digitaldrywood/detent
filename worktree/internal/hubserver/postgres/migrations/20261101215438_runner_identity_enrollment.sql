-- +goose Up
CREATE TABLE runner_enrollments (
    organization_id text NOT NULL,
    id text NOT NULL CHECK (btrim(id) <> ''),
    runner_id text NOT NULL DEFAULT '',
    machine_id text NOT NULL DEFAULT '',
    token_hash text NOT NULL CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    scope text NOT NULL CHECK (scope IN ('projects','organization')),
    shared_machine boolean NOT NULL DEFAULT false,
    operations text[] NOT NULL CHECK (cardinality(operations) BETWEEN 1 AND 5),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    created_by text NOT NULL,
    redeemed_at timestamptz,
    revoked_at timestamptz,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, token_hash),
    CHECK (expires_at > created_at),
    CHECK ((runner_id = '' AND machine_id = '') OR (runner_id <> '' AND machine_id <> ''))
);

CREATE TABLE runner_enrollment_projects (
    organization_id text NOT NULL,
    enrollment_id text NOT NULL,
    project_id text NOT NULL,
    PRIMARY KEY (organization_id, enrollment_id, project_id),
    FOREIGN KEY (organization_id, enrollment_id) REFERENCES runner_enrollments (organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE runner_identities (
    organization_id text NOT NULL,
    runner_id text NOT NULL CHECK (btrim(runner_id) <> ''),
    machine_id text NOT NULL CHECK (btrim(machine_id) <> ''),
    token_id text NOT NULL,
    enrollment_id text NOT NULL,
    scope text NOT NULL CHECK (scope IN ('projects','organization')),
    operations text[] NOT NULL CHECK (cardinality(operations) BETWEEN 1 AND 5),
    display_name text NOT NULL DEFAULT '',
    capacity_limit bigint NOT NULL CHECK (capacity_limit >= 0),
    reported_capacity bigint NOT NULL CHECK (reported_capacity >= 0),
    os text NOT NULL DEFAULT '',
    architecture text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    removed_at timestamptz,
    PRIMARY KEY (organization_id, runner_id),
    UNIQUE (organization_id, token_id),
    UNIQUE (organization_id, enrollment_id),
    FOREIGN KEY (organization_id, machine_id) REFERENCES machines (organization_id, id),
    FOREIGN KEY (organization_id, token_id) REFERENCES api_tokens (organization_id, id),
    FOREIGN KEY (organization_id, enrollment_id) REFERENCES runner_enrollments (organization_id, id)
);

CREATE INDEX runner_identities_machine_idx ON runner_identities (organization_id, machine_id, runner_id);
CREATE INDEX runner_identities_active_idx ON runner_identities (organization_id, runner_id) WHERE removed_at IS NULL;

CREATE TABLE runner_lease_assignments (
    organization_id text NOT NULL,
    lease_id text NOT NULL,
    runner_id text NOT NULL,
    assigned_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, lease_id),
    FOREIGN KEY (organization_id, lease_id) REFERENCES leases (organization_id, lease_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id)
);

CREATE INDEX runner_lease_assignments_runner_idx ON runner_lease_assignments (organization_id, runner_id, lease_id);

CREATE TABLE runner_identity_events (
    organization_id text NOT NULL,
    event_id bigint GENERATED ALWAYS AS IDENTITY,
    runner_id text NOT NULL,
    actor_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('enrolled','renewed','rotated','revoked')),
    occurred_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, event_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id)
);

CREATE INDEX runner_identity_events_runner_idx ON runner_identity_events (organization_id, runner_id, event_id DESC);

CREATE TABLE runner_repository_lists (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    request_id text NOT NULL,
    pending boolean NOT NULL,
    reason text NOT NULL DEFAULT '',
    listed_at timestamptz,
    PRIMARY KEY (organization_id, runner_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

CREATE TABLE runner_repositories (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    owner text NOT NULL,
    name text NOT NULL,
    private boolean NOT NULL,
    pushed_at timestamptz,
    listed_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, runner_id, owner, name),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);

-- +goose StatementBegin
DO $$
DECLARE
    table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['runner_enrollments','runner_enrollment_projects','runner_identities','runner_lease_assignments','runner_identity_events','runner_repository_lists','runner_repositories'] LOOP
        EXECUTE format('ALTER TABLE %I OWNER TO detent_migration', table_name);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC, detent_platform', table_name);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO detent_runtime', table_name);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting(''detent.organization_id'', true), '''')) WITH CHECK (organization_id = NULLIF(current_setting(''detent.organization_id'', true), ''''))', table_name);
    END LOOP;
END $$;
-- +goose StatementEnd

GRANT USAGE, SELECT ON SEQUENCE runner_identity_events_event_id_seq TO detent_runtime;
GRANT USAGE, SELECT ON SEQUENCE runner_identity_events_event_id_seq TO detent_migration;

-- +goose Down
REVOKE USAGE, SELECT ON SEQUENCE runner_identity_events_event_id_seq FROM detent_runtime, detent_migration;
DROP TABLE runner_repositories;
DROP TABLE runner_repository_lists;
DROP TABLE runner_identity_events;
DROP TABLE runner_lease_assignments;
DROP TABLE runner_identities;
DROP TABLE runner_enrollment_projects;
DROP TABLE runner_enrollments;
