-- +goose Up
CREATE TABLE event_compactions (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  aggregate_kind TEXT NOT NULL CHECK (aggregate_kind IN ('work_item', 'conversation')),
  aggregate_id TEXT NOT NULL,
  scanned_sequence INTEGER NOT NULL DEFAULT 0,
  summary_json TEXT NOT NULL CHECK (json_valid(summary_json)),
  PRIMARY KEY (organization_id, project_id, aggregate_kind, aggregate_id),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);
ALTER TABLE attempt_diff_body_migration ADD COLUMN event_compaction_storage_before TEXT NOT NULL DEFAULT '' CHECK (event_compaction_storage_before='' OR json_valid(event_compaction_storage_before));
CREATE INDEX collaboration_events_observation_attempt ON collaboration_events(organization_id,project_id,work_item_id,json_extract(data_json,'$.run.attempt_id'),sequence) WHERE type='run.observed';
CREATE INDEX conversation_events_message_update ON conversation_events(conversation_id,json_extract(body_json,'$.id'),seq) WHERE type='message.updated';
CREATE INDEX conversation_events_execution_update ON conversation_events(conversation_id,json_extract(body_json,'$.attempt_id'),json_extract(body_json,'$.status'),json_extract(body_json,'$.thread_id'),json_extract(body_json,'$.turn_id'),seq) WHERE type='execution.updated';
CREATE INDEX conversation_events_snapshot ON conversation_events(conversation_id,seq) WHERE type='conversation.updated';

-- +goose Down
DROP INDEX conversation_events_snapshot;
DROP INDEX conversation_events_execution_update;
DROP INDEX conversation_events_message_update;
DROP INDEX collaboration_events_observation_attempt;
ALTER TABLE attempt_diff_body_migration DROP COLUMN event_compaction_storage_before;
DROP TABLE event_compactions;
