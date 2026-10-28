-- +goose Up
CREATE INDEX collaboration_events_report_recovery ON collaboration_events(organization_id,project_id,type,id) WHERE analytics_projection_version IS NULL AND type IN ('workflow.transitioned','scheduler.decision','issue.created');

-- +goose Down
DROP INDEX collaboration_events_report_recovery;
