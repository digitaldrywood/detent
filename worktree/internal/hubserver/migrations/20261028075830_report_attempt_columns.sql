-- +goose Up
ALTER TABLE native_attempts ADD COLUMN report_projection_version INTEGER;
ALTER TABLE native_attempts ADD COLUMN report_requested_model TEXT;
ALTER TABLE native_attempts ADD COLUMN report_resolved_model TEXT;
ALTER TABLE native_attempts ADD COLUMN report_failed_at TEXT;
CREATE INDEX native_attempts_report_conversion ON native_attempts(id) WHERE report_projection_version IS NULL;
CREATE INDEX native_attempts_report_scope_pending ON native_attempts(organization_id,project_id,work_item_id,id) WHERE report_projection_version IS NULL;
CREATE INDEX native_attempts_report_failures ON native_attempts(organization_id,project_id,report_failed_at,id) WHERE report_failed_at IS NOT NULL;
CREATE INDEX collaboration_events_report_scope_pending ON collaboration_events(organization_id,project_id,id) WHERE analytics_projection_version IS NULL;

-- +goose Down
DROP INDEX collaboration_events_report_scope_pending;
DROP INDEX native_attempts_report_failures;
DROP INDEX native_attempts_report_scope_pending;
DROP INDEX native_attempts_report_conversion;
ALTER TABLE native_attempts DROP COLUMN report_failed_at;
ALTER TABLE native_attempts DROP COLUMN report_resolved_model;
ALTER TABLE native_attempts DROP COLUMN report_requested_model;
ALTER TABLE native_attempts DROP COLUMN report_projection_version;
