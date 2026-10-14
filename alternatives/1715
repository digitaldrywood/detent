-- +goose Up
ALTER TABLE health_detector_ticks ADD COLUMN baseline_unavailable_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(baseline_unavailable_json));
CREATE INDEX health_rate_attempt_window ON native_attempts(organization_id,project_id,julianday(updated_at));

-- +goose Down
DROP INDEX health_rate_attempt_window;
ALTER TABLE health_detector_ticks DROP COLUMN baseline_unavailable_json;
