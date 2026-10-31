-- +goose Up
ALTER TABLE conversation_tool_revisions ADD COLUMN conversion_state INTEGER NOT NULL DEFAULT 0;
CREATE INDEX storage_discovery_tool_revisions ON conversation_tool_revisions(conversion_state) WHERE conversion_state=0;
CREATE INDEX storage_queued_tool_revisions ON conversation_tool_revisions(conversation_id) WHERE conversion_state=1;
DROP INDEX storage_conversion_schedule;
CREATE INDEX storage_conversion_schedule ON storage_conversion_progress(updated_at,organization_id,project_id,family,aggregate_id) WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files','tool_revisions');

-- +goose Down
DROP INDEX storage_conversion_schedule;
CREATE INDEX storage_conversion_schedule ON storage_conversion_progress(updated_at,organization_id,project_id,family,aggregate_id) WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files');
DROP INDEX storage_queued_tool_revisions;
DROP INDEX storage_discovery_tool_revisions;
ALTER TABLE conversation_tool_revisions DROP COLUMN conversion_state;
