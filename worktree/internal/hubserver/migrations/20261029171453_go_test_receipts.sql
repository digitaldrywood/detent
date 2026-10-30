-- +goose Up
CREATE TABLE go_test_receipts (
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    binary_key TEXT NOT NULL,
    inputs_digest TEXT NOT NULL,
    inputs TEXT NOT NULL,
    package TEXT NOT NULL,
    runner_id TEXT NOT NULL,
    recorded_at_ns INTEGER NOT NULL,
    PRIMARY KEY (organization_id, project_id, binary_key, inputs_digest)
);
CREATE INDEX go_test_receipts_recorded ON go_test_receipts(recorded_at_ns);
CREATE INDEX go_test_receipts_lookup ON go_test_receipts(organization_id, project_id, binary_key, recorded_at_ns DESC);

-- +goose Down
DROP TABLE go_test_receipts;
