-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN scheduler_outcome TEXT;
ALTER TABLE collaboration_events ADD COLUMN scheduler_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN scheduler_source TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN scheduler_runner_id TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS collaboration_events_scheduler_outcome_idx;
CREATE INDEX collaboration_events_scheduler_outcome_idx ON collaboration_events(organization_id,project_id,work_item_id,scheduler_outcome,sequence DESC) WHERE type='scheduler.decision';
CREATE INDEX collaboration_events_scheduler_conversion_idx ON collaboration_events(id) WHERE type='scheduler.decision' AND scheduler_outcome IS NULL;
CREATE INDEX collaboration_events_scheduler_runner_time_idx ON collaboration_events(organization_id,project_id,work_item_id,scheduler_runner_id,recorded_at) WHERE type='scheduler.decision';
DROP TRIGGER collaboration_events_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE OF id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at ON collaboration_events
BEGIN
 SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER collaboration_events_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
BEGIN
 SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd
DROP INDEX collaboration_events_scheduler_conversion_idx;
DROP INDEX collaboration_events_scheduler_runner_time_idx;
DROP INDEX IF EXISTS collaboration_events_scheduler_outcome_idx;
ALTER TABLE collaboration_events DROP COLUMN scheduler_runner_id;
ALTER TABLE collaboration_events DROP COLUMN scheduler_source;
ALTER TABLE collaboration_events DROP COLUMN scheduler_reason;
ALTER TABLE collaboration_events DROP COLUMN scheduler_outcome;
