-- +goose Up
CREATE INDEX health_usage_current_read_idx ON report_usage_current(organization_id,project_id,period,source_key);
CREATE INDEX health_usage_legacy_read_idx ON attempt_usage(organization_id,project_id,period,attempt_id) WHERE upper(currency)='USD';

-- +goose Down
DROP INDEX health_usage_legacy_read_idx;
DROP INDEX health_usage_current_read_idx;
