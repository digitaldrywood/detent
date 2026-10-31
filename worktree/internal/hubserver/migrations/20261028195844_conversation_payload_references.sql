-- +goose Up
ALTER TABLE conversation_tool_revisions ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_events ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_messages ADD COLUMN actor_principal_id TEXT;
ALTER TABLE conversation_messages ADD COLUMN text_bytes INTEGER;
ALTER TABLE conversation_events ADD COLUMN delta_offset INTEGER;
ALTER TABLE conversation_events ADD COLUMN delta_bytes INTEGER;
ALTER TABLE conversation_events ADD COLUMN message_revision INTEGER;
CREATE INDEX conversation_events_payload_pending ON conversation_events(conversation_id) WHERE content_attachment_id='';
CREATE INDEX conversation_messages_payload_pending ON conversation_messages(conversation_id) WHERE actor_principal_id IS NULL;

CREATE INDEX storage_conversion_schedule ON storage_conversion_progress(updated_at,organization_id,project_id,family,aggregate_id) WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files');

DROP TRIGGER storage_conversion_enqueue_conversation_events;
DROP TRIGGER storage_conversion_enqueue_messages;
-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_conversation_events AFTER INSERT ON conversation_events
WHEN NEW.content_attachment_id=''
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'conversation_events',r.conversation_id,1,r.rowid,'' FROM conversation_events r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.type IN ('message.accepted','message.updated')) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=0,
 format_version=1;
UPDATE conversation_events SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_messages AFTER INSERT ON conversation_messages
WHEN NEW.content_attachment_id=''
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'messages',r.conversation_id,1,r.rowid,'' FROM conversation_messages r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.content_attachment_id='') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=0,
 format_version=1;
UPDATE conversation_messages SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd

CREATE INDEX conversation_events_payload_discovery ON conversation_events(content_attachment_id) WHERE content_attachment_id='' AND conversion_state<>1;
CREATE INDEX conversation_messages_payload_discovery ON conversation_messages(actor_principal_id) WHERE actor_principal_id IS NULL AND conversion_state<>1;
CREATE INDEX conversation_tool_revisions_payload_pending ON conversation_tool_revisions(conversation_id,message_id,revision) WHERE content_attachment_id='';

-- +goose Down
DROP INDEX conversation_tool_revisions_payload_pending;
DROP INDEX conversation_messages_payload_discovery;
DROP INDEX conversation_events_payload_discovery;

DROP TRIGGER storage_conversion_enqueue_conversation_events;
DROP TRIGGER storage_conversion_enqueue_messages;
-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_conversation_events AFTER INSERT ON conversation_events
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'conversation_events',r.conversation_id,1,r.rowid,'' FROM conversation_events r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.type IN ('message.accepted','message.updated')) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=0,
 format_version=1;
UPDATE conversation_events SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_messages AFTER INSERT ON conversation_messages
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'messages',r.conversation_id,1,r.rowid,'' FROM conversation_messages r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.content_attachment_id='') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=0,
 format_version=1;
UPDATE conversation_messages SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd

DROP INDEX storage_conversion_schedule;
DROP INDEX conversation_messages_payload_pending;
DROP INDEX conversation_events_payload_pending;
ALTER TABLE conversation_events DROP COLUMN message_revision;
ALTER TABLE conversation_events DROP COLUMN delta_bytes;
ALTER TABLE conversation_events DROP COLUMN delta_offset;
ALTER TABLE conversation_messages DROP COLUMN text_bytes;
ALTER TABLE conversation_messages DROP COLUMN actor_principal_id;
ALTER TABLE conversation_events DROP COLUMN content_attachment_id;
ALTER TABLE conversation_tool_revisions DROP COLUMN content_attachment_id;
