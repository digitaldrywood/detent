-- +goose Up
ALTER TABLE change_content_objects ADD COLUMN billable INTEGER NOT NULL DEFAULT 1 CHECK (billable IN (0,1));
DROP TRIGGER change_content_usage_insert;
DROP TRIGGER change_content_usage_delete;
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_insert AFTER INSERT ON change_content_objects WHEN NEW.billable=1 BEGIN
    UPDATE hosted_usage_counters SET value=value+NEW.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_delete AFTER DELETE ON change_content_objects WHEN OLD.billable=1 BEGIN
    UPDATE hosted_usage_counters SET value=value-OLD.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_update AFTER UPDATE OF billable ON change_content_objects WHEN NEW.billable<>OLD.billable BEGIN
    UPDATE hosted_usage_counters SET value=value+(NEW.billable-OLD.billable)*NEW.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd
UPDATE change_content_objects SET billable=0
WHERE (organization_id,object_key) IN (
    SELECT organization_id,response_object_key FROM native_commands WHERE operation LIKE 'attachment.%'
)
AND object_key NOT IN (
    SELECT object_key FROM change_sources
    UNION SELECT object_key FROM attempt_diffs
    UNION SELECT object_key FROM change_command_outputs
    UNION SELECT payload_object_key FROM native_attempts
    UNION SELECT checkpoint_object_key FROM native_attempts
    UNION SELECT payload_object_key FROM change_versions
    UNION SELECT payload_object_key FROM change_evidence
    UNION SELECT payload_object_key FROM collaboration_events
    UNION SELECT payload_object_key FROM change_landing_receipts
    UNION SELECT payload_object_key FROM analytics_hourly_rollups
    UNION SELECT object_key FROM attachments
);

-- +goose Down
UPDATE change_content_objects SET billable=1 WHERE billable=0;
DROP TRIGGER change_content_usage_update;
DROP TRIGGER change_content_usage_insert;
DROP TRIGGER change_content_usage_delete;
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_insert AFTER INSERT ON change_content_objects BEGIN
    UPDATE hosted_usage_counters SET value=value+NEW.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_content_usage_delete AFTER DELETE ON change_content_objects BEGIN
    UPDATE hosted_usage_counters SET value=value-OLD.object_bytes WHERE name='collaboration_bytes';
END;
-- +goose StatementEnd
ALTER TABLE change_content_objects DROP COLUMN billable;
