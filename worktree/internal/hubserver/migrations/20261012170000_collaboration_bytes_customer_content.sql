-- +goose Up
CREATE INDEX native_commands_created ON native_commands(julianday(created_at));

DROP TRIGGER hosted_usage_events_insert;
DROP TRIGGER hosted_usage_events_update;
DROP TRIGGER hosted_usage_events_delete;
DROP TRIGGER hosted_usage_commands_insert;
DROP TRIGGER hosted_usage_commands_update;
DROP TRIGGER hosted_usage_commands_delete;
DROP TRIGGER hosted_usage_attempts_insert;
DROP TRIGGER hosted_usage_attempts_update;
DROP TRIGGER hosted_usage_attempts_delete;

-- +goose StatementBegin
CREATE TRIGGER hosted_usage_events_insert AFTER INSERT ON collaboration_events BEGIN
  UPDATE hosted_usage_counters SET value = value + 1 WHERE name IN ('events_total', 'history_records');
  UPDATE hosted_usage_counters SET value = value + coalesce(length(CAST(NEW.data_json AS BLOB))+length(CAST(NEW.actor_json AS BLOB)),0)
  WHERE name = 'collaboration_bytes' AND NEW.type NOT LIKE 'run.%' AND NEW.type <> 'scheduler.decision';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_events_update AFTER UPDATE OF data_json, actor_json ON collaboration_events BEGIN
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.data_json AS BLOB))+length(CAST(OLD.actor_json AS BLOB)),0) + coalesce(length(CAST(NEW.data_json AS BLOB))+length(CAST(NEW.actor_json AS BLOB)),0)
  WHERE name = 'collaboration_bytes' AND NEW.type NOT LIKE 'run.%' AND NEW.type <> 'scheduler.decision';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_events_delete AFTER DELETE ON collaboration_events BEGIN
  UPDATE hosted_usage_counters SET value = value - 1 WHERE name IN ('events_total', 'history_records');
  UPDATE hosted_usage_counters SET value = value - coalesce(length(CAST(OLD.data_json AS BLOB))+length(CAST(OLD.actor_json AS BLOB)),0)
  WHERE name = 'collaboration_bytes' AND OLD.type NOT LIKE 'run.%' AND OLD.type <> 'scheduler.decision';
END;
-- +goose StatementEnd

UPDATE hosted_usage_counters SET value =
    (SELECT coalesce(sum(length(CAST(title AS BLOB))+length(CAST(body AS BLOB))+length(CAST(labels_json AS BLOB))+length(CAST(assignees_json AS BLOB))),0) FROM issues) +
    (SELECT coalesce(sum(length(CAST(body AS BLOB))),0) FROM native_comments) +
    (SELECT coalesce(sum(length(CAST(record_json AS BLOB))),0) FROM collaboration_versions) +
    (SELECT coalesce(sum(length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB))),0) FROM collaboration_events WHERE type NOT LIKE 'run.%' AND type <> 'scheduler.decision') +
    (SELECT coalesce(sum(length(CAST(reference_json AS BLOB))),0) FROM artifact_references) +
    (SELECT coalesce(sum(length(bundle)),0) FROM change_sources) +
    (SELECT coalesce(sum(length(CAST(record_json AS BLOB))),0) FROM github_import_records) +
    (SELECT coalesce(sum(size),0) FROM attachments WHERE object_deleted_at IS NULL)
WHERE name = 'collaboration_bytes';

-- +goose Down
DROP INDEX native_commands_created;
DROP TRIGGER hosted_usage_events_insert;
DROP TRIGGER hosted_usage_events_update;
DROP TRIGGER hosted_usage_events_delete;

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

UPDATE hosted_usage_counters SET value =
    (SELECT coalesce(sum(length(CAST(title AS BLOB))+length(CAST(body AS BLOB))+length(CAST(labels_json AS BLOB))+length(CAST(assignees_json AS BLOB))),0) FROM issues) +
    (SELECT coalesce(sum(length(CAST(body AS BLOB))),0) FROM native_comments) +
    (SELECT coalesce(sum(length(CAST(record_json AS BLOB))),0) FROM collaboration_versions) +
    (SELECT coalesce(sum(length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB))),0) FROM collaboration_events) +
    (SELECT coalesce(sum(length(CAST(response_json AS BLOB))),0) FROM native_commands) +
    (SELECT coalesce(sum(length(CAST(data_json AS BLOB))),0) FROM native_attempts) +
    (SELECT coalesce(sum(length(CAST(reference_json AS BLOB))),0) FROM artifact_references) +
    (SELECT coalesce(sum(length(bundle)),0) FROM change_sources) +
    (SELECT coalesce(sum(length(CAST(record_json AS BLOB))),0) FROM github_import_records) +
    (SELECT coalesce(sum(size),0) FROM attachments WHERE object_deleted_at IS NULL)
WHERE name = 'collaboration_bytes';
