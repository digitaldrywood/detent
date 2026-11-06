-- +goose Up
CREATE TABLE runner_update_requests (
    organization_id text NOT NULL REFERENCES organizations(organization_id),
    request_id text NOT NULL,
    runner_id text,
    requested_at timestamptz NOT NULL,
    service text NOT NULL CHECK(service = 'detent'),
    expected_build_revision text NOT NULL DEFAULT '',
    version text NOT NULL CHECK(btrim(version) <> ''),
    release boolean NOT NULL,
    from_release boolean NOT NULL,
    urgent boolean NOT NULL,
    follow_hub boolean NOT NULL,
    PRIMARY KEY(organization_id, request_id),
    UNIQUE(organization_id, runner_id, request_id),
    FOREIGN KEY(organization_id, runner_id) REFERENCES runner_identities(organization_id, runner_id) ON DELETE CASCADE,
    CHECK((urgent AND runner_id IS NULL AND release AND expected_build_revision = '' AND NOT follow_hub) OR
          (NOT urgent AND runner_id IS NOT NULL AND expected_build_revision <> '')),
    CHECK(NOT from_release OR release),
    CHECK(NOT follow_hub OR release)
);
CREATE INDEX runner_update_requests_runner_idx ON runner_update_requests(organization_id, runner_id, requested_at DESC, request_id);

CREATE TABLE runner_update_desired (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    request_id text NOT NULL,
    PRIMARY KEY(organization_id, runner_id),
    FOREIGN KEY(organization_id, runner_id) REFERENCES runner_identities(organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY(organization_id, runner_id, request_id) REFERENCES runner_update_requests(organization_id, runner_id, request_id)
);

CREATE TABLE runner_update_observations (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    discovery text NOT NULL CHECK(discovery IN ('unknown','available','up_to_date')),
    protocol integer NOT NULL CHECK(protocol = 1),
    service text NOT NULL CHECK(service = 'detent'),
    revision text NOT NULL,
    supported boolean NOT NULL,
    pending boolean NOT NULL,
    available_observed_at timestamptz,
    available_version text NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    running_version text NOT NULL,
    running_commit text NOT NULL,
    running_source text NOT NULL,
    running_sha256 text NOT NULL DEFAULT '',
    running_os text NOT NULL,
    running_architecture text NOT NULL,
    running_verified_release boolean NOT NULL,
    running_observed_at timestamptz NOT NULL,
    PRIMARY KEY(organization_id, runner_id),
    FOREIGN KEY(organization_id, runner_id) REFERENCES runner_identities(organization_id, runner_id) ON DELETE CASCADE
);

CREATE TABLE runner_update_receipts (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    request_id text NOT NULL,
    execution_artifact boolean NOT NULL,
    drain_reason text NOT NULL DEFAULT '',
    failure_reason text NOT NULL DEFAULT '',
    status text NOT NULL CHECK(status IN ('applying','draining','refused','uncertain','applied','restart_requested','running')),
    observed_at timestamptz NOT NULL,
    previous_failure_request_id text,
    previous_failure_reason text,
    previous_failure_observed_at timestamptz,
    PRIMARY KEY(organization_id, runner_id),
    FOREIGN KEY(organization_id, runner_id) REFERENCES runner_identities(organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY(organization_id, runner_id, request_id) REFERENCES runner_update_requests(organization_id, runner_id, request_id),
    FOREIGN KEY(organization_id, runner_id, previous_failure_request_id) REFERENCES runner_update_requests(organization_id, runner_id, request_id),
    CHECK((previous_failure_request_id IS NULL AND previous_failure_reason IS NULL AND previous_failure_observed_at IS NULL) OR
          (previous_failure_request_id IS NOT NULL AND previous_failure_reason IS NOT NULL AND previous_failure_observed_at IS NOT NULL))
);

CREATE TABLE runner_update_build_evidence (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    evidence_role text NOT NULL CHECK(evidence_role IN ('receipt_running','receipt_applied','receipt_verified_target')),
    version text NOT NULL,
    commit_id text NOT NULL,
    source text NOT NULL,
    sha256 text NOT NULL DEFAULT '',
    os text NOT NULL,
    architecture text NOT NULL,
    verified_release boolean NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY(organization_id, runner_id, evidence_role),
    FOREIGN KEY(organization_id, runner_id) REFERENCES runner_identities(organization_id, runner_id) ON DELETE CASCADE
);

CREATE TABLE runner_urgent_updates (
    organization_id text PRIMARY KEY REFERENCES organizations(organization_id),
    revision bigint NOT NULL CHECK(revision >= 0),
    request_id text,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY(organization_id, request_id) REFERENCES runner_update_requests(organization_id, request_id)
);

-- +goose StatementBegin
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['runner_update_requests','runner_update_desired','runner_update_observations','runner_update_receipts','runner_update_build_evidence','runner_urgent_updates'] LOOP
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
DROP TABLE runner_urgent_updates;
DROP TABLE runner_update_build_evidence;
DROP TABLE runner_update_receipts;
DROP TABLE runner_update_observations;
DROP TABLE runner_update_desired;
DROP TABLE runner_update_requests;
