-- +goose Up
CREATE TABLE hosted_usage_counters (
  name TEXT PRIMARY KEY,
  value INTEGER NOT NULL
) STRICT;

INSERT INTO hosted_usage_counters(name, value) VALUES
  ('events_total', (SELECT count(*) FROM collaboration_events)),
  ('history_records', (SELECT count(*) FROM collaboration_events) + (SELECT count(*) FROM collaboration_versions) + (SELECT count(*) FROM native_attempt_events)),
  ('collaboration_bytes',
    (SELECT coalesce(sum(length(CAST(title AS BLOB))+length(CAST(body AS BLOB))+length(CAST(labels_json AS BLOB))+length(CAST(assignees_json AS BLOB))),0) FROM issues) +
    (SELECT coalesce(sum(length(CAST(body AS BLOB))),0) FROM native_comments) +
    (SELECT coalesce(sum(length(CAST(record_json AS BLOB))),0) FROM collaboration_versions) +
    (SELECT coalesce(sum(length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB))),0) FROM collaboration_events) +
    (SELECT coalesce(sum(length(CAST(response_json AS BLOB))),0) FROM native_commands) +
    (SELECT coalesce(sum(length(CAST(data_json AS BLOB))),0) FROM native_attempts) +
    (SELECT coalesce(sum(length(CAST(reference_json AS BLOB))),0) FROM artifact_references) +
    (SELECT coalesce(sum(length(bundle)),0) FROM change_sources) +
    (SELECT coalesce(sum(length(CAST(record_json AS BLOB))),0) FROM github_import_records) +
    (SELECT coalesce(sum(size),0) FROM attachments WHERE object_deleted_at IS NULL));

-- +goose StatementBegin
CREATE TRIGGER hosted_usage_events_insert AFTER INSERT ON collaboration_events BEGIN
  UPDATE hosted_usage_counters SET value = value + 1 WHERE name IN ('events_total', 'history_records');
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.data_json AS BLOB))+length(CAST(NEW.actor_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_events_update AFTER UPDATE OF data_json, actor_json ON collaboration_events BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.data_json AS BLOB))+length(CAST(OLD.actor_json AS BLOB)),0) + coalesce(length(CAST(NEW.data_json AS BLOB))+length(CAST(NEW.actor_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_events_delete AFTER DELETE ON collaboration_events BEGIN
  UPDATE hosted_usage_counters SET value = value - 1 WHERE name IN ('events_total', 'history_records');
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.data_json AS BLOB))+length(CAST(OLD.actor_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_versions_insert AFTER INSERT ON collaboration_versions BEGIN
  UPDATE hosted_usage_counters SET value = value + 1 WHERE name = 'history_records';
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.record_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_versions_update AFTER UPDATE OF record_json ON collaboration_versions BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.record_json AS BLOB)),0) + coalesce(length(CAST(NEW.record_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_versions_delete AFTER DELETE ON collaboration_versions BEGIN
  UPDATE hosted_usage_counters SET value = value - 1 WHERE name = 'history_records';
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.record_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attempt_events_insert AFTER INSERT ON native_attempt_events BEGIN
  UPDATE hosted_usage_counters SET value = value + 1 WHERE name = 'history_records';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attempt_events_delete AFTER DELETE ON native_attempt_events BEGIN
  UPDATE hosted_usage_counters SET value = value - 1 WHERE name = 'history_records';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_issues_insert AFTER INSERT ON issues BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.title AS BLOB))+length(CAST(NEW.body AS BLOB))+length(CAST(NEW.labels_json AS BLOB))+length(CAST(NEW.assignees_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_issues_update AFTER UPDATE OF title, body, labels_json, assignees_json ON issues BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.title AS BLOB))+length(CAST(OLD.body AS BLOB))+length(CAST(OLD.labels_json AS BLOB))+length(CAST(OLD.assignees_json AS BLOB)),0) + coalesce(length(CAST(NEW.title AS BLOB))+length(CAST(NEW.body AS BLOB))+length(CAST(NEW.labels_json AS BLOB))+length(CAST(NEW.assignees_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_issues_delete AFTER DELETE ON issues BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.title AS BLOB))+length(CAST(OLD.body AS BLOB))+length(CAST(OLD.labels_json AS BLOB))+length(CAST(OLD.assignees_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_comments_insert AFTER INSERT ON native_comments BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.body AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_comments_update AFTER UPDATE OF body ON native_comments BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.body AS BLOB)),0) + coalesce(length(CAST(NEW.body AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_comments_delete AFTER DELETE ON native_comments BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.body AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_commands_insert AFTER INSERT ON native_commands BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.response_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_commands_update AFTER UPDATE OF response_json ON native_commands BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.response_json AS BLOB)),0) + coalesce(length(CAST(NEW.response_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_commands_delete AFTER DELETE ON native_commands BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.response_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attempts_insert AFTER INSERT ON native_attempts BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.data_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attempts_update AFTER UPDATE OF data_json ON native_attempts BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.data_json AS BLOB)),0) + coalesce(length(CAST(NEW.data_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attempts_delete AFTER DELETE ON native_attempts BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.data_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_references_insert AFTER INSERT ON artifact_references BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.reference_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_references_update AFTER UPDATE OF reference_json ON artifact_references BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.reference_json AS BLOB)),0) + coalesce(length(CAST(NEW.reference_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_references_delete AFTER DELETE ON artifact_references BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.reference_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_sources_insert AFTER INSERT ON change_sources BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(NEW.bundle),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_sources_update AFTER UPDATE OF bundle ON change_sources BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(OLD.bundle),0) + coalesce(length(NEW.bundle),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_sources_delete AFTER DELETE ON change_sources BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(OLD.bundle),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_imports_insert AFTER INSERT ON github_import_records BEGIN
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.record_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_imports_update AFTER UPDATE OF record_json ON github_import_records BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.record_json AS BLOB)),0) + coalesce(length(CAST(NEW.record_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_imports_delete AFTER DELETE ON github_import_records BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.record_json AS BLOB)),0) WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attachments_insert AFTER INSERT ON attachments BEGIN
  UPDATE hosted_usage_counters SET value = value + CASE WHEN NEW.object_deleted_at IS NULL THEN coalesce(NEW.size,0) ELSE 0 END WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attachments_update AFTER UPDATE OF size, object_deleted_at ON attachments BEGIN
  UPDATE hosted_usage_counters SET value = value - CASE WHEN OLD.object_deleted_at IS NULL THEN coalesce(OLD.size,0) ELSE 0 END + CASE WHEN NEW.object_deleted_at IS NULL THEN coalesce(NEW.size,0) ELSE 0 END WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attachments_delete AFTER DELETE ON attachments BEGIN
  UPDATE hosted_usage_counters SET value = value - CASE WHEN OLD.object_deleted_at IS NULL THEN coalesce(OLD.size,0) ELSE 0 END WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER hosted_usage_attachments_delete;
DROP TRIGGER hosted_usage_attachments_update;
DROP TRIGGER hosted_usage_attachments_insert;
DROP TRIGGER hosted_usage_imports_delete;
DROP TRIGGER hosted_usage_imports_update;
DROP TRIGGER hosted_usage_imports_insert;
DROP TRIGGER hosted_usage_sources_delete;
DROP TRIGGER hosted_usage_sources_update;
DROP TRIGGER hosted_usage_sources_insert;
DROP TRIGGER hosted_usage_references_delete;
DROP TRIGGER hosted_usage_references_update;
DROP TRIGGER hosted_usage_references_insert;
DROP TRIGGER hosted_usage_attempts_delete;
DROP TRIGGER hosted_usage_attempts_update;
DROP TRIGGER hosted_usage_attempts_insert;
DROP TRIGGER hosted_usage_commands_delete;
DROP TRIGGER hosted_usage_commands_update;
DROP TRIGGER hosted_usage_commands_insert;
DROP TRIGGER hosted_usage_comments_delete;
DROP TRIGGER hosted_usage_comments_update;
DROP TRIGGER hosted_usage_comments_insert;
DROP TRIGGER hosted_usage_issues_delete;
DROP TRIGGER hosted_usage_issues_update;
DROP TRIGGER hosted_usage_issues_insert;
DROP TRIGGER hosted_usage_attempt_events_delete;
DROP TRIGGER hosted_usage_attempt_events_insert;
DROP TRIGGER hosted_usage_versions_delete;
DROP TRIGGER hosted_usage_versions_update;
DROP TRIGGER hosted_usage_versions_insert;
DROP TRIGGER hosted_usage_events_delete;
DROP TRIGGER hosted_usage_events_update;
DROP TRIGGER hosted_usage_events_insert;
DROP TABLE hosted_usage_counters;
