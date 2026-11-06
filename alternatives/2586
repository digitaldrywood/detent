-- +goose Up

CREATE TABLE go_test_receipts (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    binary_key text NOT NULL CHECK (binary_key ~ '^[0-9a-f]{64}$'),
    inputs_digest text NOT NULL CHECK (inputs_digest ~ '^[0-9a-f]{64}$'),
    inputs text NOT NULL CHECK (octet_length(inputs) <= 98304),
    package text NOT NULL CHECK (octet_length(package) <= 512),
    runner_id text NOT NULL,
    recorded_at timestamptz NOT NULL,
    attestation_id text NOT NULL CHECK (attestation_id <> ''),
    PRIMARY KEY (organization_id, project_id, binary_key, inputs_digest),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);
CREATE INDEX go_test_receipts_lookup_idx ON go_test_receipts (organization_id, project_id, binary_key, recorded_at DESC);
CREATE INDEX go_test_receipts_recorded_idx ON go_test_receipts (organization_id, recorded_at);

CREATE TABLE go_test_receipt_attestations (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    id text NOT NULL CHECK (id <> '' AND octet_length(id) <= 128),
    credential_id text NOT NULL,
    runner_id text NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);
CREATE INDEX go_test_receipt_attestations_expiry_idx ON go_test_receipt_attestations (organization_id, expires_at);
CREATE INDEX go_test_receipt_attestations_owner_idx ON go_test_receipt_attestations (organization_id, project_id, credential_id, id);

ALTER TABLE go_test_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE go_test_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON go_test_receipts FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON go_test_receipts TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON go_test_receipts TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON go_test_receipts TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE go_test_receipt_attestations ENABLE ROW LEVEL SECURITY;
ALTER TABLE go_test_receipt_attestations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON go_test_receipt_attestations FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON go_test_receipt_attestations TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON go_test_receipt_attestations TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON go_test_receipt_attestations TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE go_test_receipt_attestations;
DROP TABLE go_test_receipts;
