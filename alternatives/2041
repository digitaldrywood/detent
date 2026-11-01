-- +goose Up
CREATE TABLE go_test_receipt_attestations (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    runner_id TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL
);
CREATE INDEX go_test_receipt_attestations_expiry ON go_test_receipt_attestations(expires_at_ns);
ALTER TABLE go_test_receipts ADD COLUMN attestation_id TEXT NOT NULL DEFAULT '';
CREATE INDEX go_test_receipts_attested_lookup ON go_test_receipts(organization_id, project_id, binary_key, recorded_at_ns DESC) WHERE attestation_id <> '';

-- +goose Down
DROP INDEX go_test_receipts_attested_lookup;
ALTER TABLE go_test_receipts DROP COLUMN attestation_id;
DROP TABLE go_test_receipt_attestations;
