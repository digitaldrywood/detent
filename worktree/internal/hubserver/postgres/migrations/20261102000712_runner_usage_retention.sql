-- +goose Up
CREATE INDEX hosted_runner_usage_receipts_retention_idx ON hosted_usage_receipts (organization_id, window_start, receipt_id) WHERE metric IN ('api_mutations','heartbeats','ingested_events');
CREATE INDEX hosted_runner_usage_windows_retention_idx ON hosted_usage_windows (organization_id, window_start, metric) WHERE metric IN ('api_mutations','heartbeats','ingested_events');

-- +goose Down
DROP INDEX hosted_runner_usage_windows_retention_idx;
DROP INDEX hosted_runner_usage_receipts_retention_idx;
