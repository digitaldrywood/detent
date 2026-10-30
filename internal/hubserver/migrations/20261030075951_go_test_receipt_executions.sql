-- +goose Up
CREATE TABLE go_test_receipt_executions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    runner_id TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL
);
CREATE INDEX go_test_receipt_executions_created ON go_test_receipt_executions(created_at_ns);
ALTER TABLE go_test_receipts ADD COLUMN execution_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE go_test_receipts DROP COLUMN execution_id;
DROP TABLE go_test_receipt_executions;
