-- +goose Up
CREATE INDEX hosted_http_usage_receipts_retention_idx ON hosted_usage_receipts (organization_id, window_start, receipt_id) WHERE metric IN ('http_requests','http_duration_microseconds','http_response_bytes','http_request_bytes','http_request_bytes_known');
CREATE INDEX hosted_http_usage_windows_retention_idx ON hosted_usage_windows (organization_id, window_start, metric) WHERE metric IN ('http_requests','http_duration_microseconds','http_response_bytes','http_request_bytes','http_request_bytes_known');
GRANT DELETE ON hosted_usage_receipts TO detent_runtime;

-- +goose Down
REVOKE DELETE ON hosted_usage_receipts FROM detent_runtime;
DROP INDEX hosted_http_usage_receipts_retention_idx;
DROP INDEX hosted_http_usage_windows_retention_idx;
