-- +goose Up
CREATE INDEX native_commands_attachment_objects ON native_commands(organization_id,response_object_key) WHERE response_object_key<>'' AND operation GLOB 'attachment.*';
CREATE INDEX native_commands_billable_objects ON native_commands(organization_id,response_object_key) WHERE response_object_key<>'' AND operation NOT GLOB 'attachment.*';
CREATE INDEX change_sources_objects ON change_sources(object_key) WHERE object_key<>'';
CREATE INDEX attempt_diffs_objects ON attempt_diffs(object_key) WHERE object_key<>'';
CREATE INDEX change_command_outputs_objects ON change_command_outputs(object_key) WHERE object_key<>'';
CREATE INDEX native_attempts_payload_objects ON native_attempts(payload_object_key) WHERE payload_object_key<>'';
CREATE INDEX native_attempts_checkpoint_objects ON native_attempts(checkpoint_object_key) WHERE checkpoint_object_key<>'';
CREATE INDEX change_versions_payload_objects ON change_versions(payload_object_key) WHERE payload_object_key<>'';
CREATE INDEX change_evidence_payload_objects ON change_evidence(payload_object_key) WHERE payload_object_key<>'';
CREATE INDEX collaboration_events_payload_objects ON collaboration_events(payload_object_key) WHERE payload_object_key<>'';
CREATE INDEX change_landing_receipts_payload_objects ON change_landing_receipts(payload_object_key) WHERE payload_object_key<>'';
CREATE INDEX analytics_hourly_rollups_payload_objects ON analytics_hourly_rollups(payload_object_key) WHERE payload_object_key<>'';
CREATE INDEX attachments_objects ON attachments(object_key) WHERE object_key<>'';

-- +goose Down
DROP INDEX attachments_objects;
DROP INDEX analytics_hourly_rollups_payload_objects;
DROP INDEX change_landing_receipts_payload_objects;
DROP INDEX collaboration_events_payload_objects;
DROP INDEX change_evidence_payload_objects;
DROP INDEX change_versions_payload_objects;
DROP INDEX native_attempts_checkpoint_objects;
DROP INDEX native_attempts_payload_objects;
DROP INDEX change_command_outputs_objects;
DROP INDEX attempt_diffs_objects;
DROP INDEX change_sources_objects;
DROP INDEX native_commands_billable_objects;
DROP INDEX native_commands_attachment_objects;
