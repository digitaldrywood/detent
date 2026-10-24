-- +goose Up
ALTER TABLE issues ADD COLUMN report_context_json TEXT;

-- +goose Down
ALTER TABLE issues DROP COLUMN report_context_json;
