-- +goose Up
UPDATE issues
SET actor_json = json_set(actor_json, '$.kind', 'integration')
WHERE json_extract(actor_json, '$.kind') = 'system'
  AND json_extract(actor_json, '$.principal_id') = 'health_detector';

UPDATE native_comments
SET actor_json = json_set(actor_json, '$.kind', 'integration')
WHERE json_extract(actor_json, '$.kind') = 'system'
  AND json_extract(actor_json, '$.principal_id') = 'health_detector';

UPDATE native_comments
SET edited_by_json = json_set(edited_by_json, '$.kind', 'integration')
WHERE json_extract(edited_by_json, '$.kind') = 'system'
  AND json_extract(edited_by_json, '$.principal_id') = 'health_detector';

DROP TRIGGER collaboration_events_no_update;
UPDATE collaboration_events
SET actor_json = json_set(actor_json, '$.kind', 'integration')
WHERE json_extract(actor_json, '$.kind') = 'system'
  AND json_extract(actor_json, '$.principal_id') = 'health_detector';
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd

DROP TRIGGER collaboration_versions_no_update;
UPDATE collaboration_versions
SET record_json = json_set(record_json, '$.actor.kind', 'integration')
WHERE json_extract(record_json, '$.actor.kind') = 'system'
  AND json_extract(record_json, '$.actor.principal_id') = 'health_detector';
UPDATE collaboration_versions
SET record_json = json_set(record_json, '$.edited_by.kind', 'integration')
WHERE json_extract(record_json, '$.edited_by.kind') = 'system'
  AND json_extract(record_json, '$.edited_by.principal_id') = 'health_detector';
-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_update BEFORE UPDATE ON collaboration_versions
BEGIN
  SELECT RAISE(ABORT, 'collaboration history is append-only');
END;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
