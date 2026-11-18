-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE issue_storage_archives (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 issue_revision bigint NOT NULL CHECK (issue_revision>0),
 event_sequence bigint NOT NULL CHECK (event_sequence>=0),
 conversation_sequence bigint NOT NULL CHECK (conversation_sequence>=0),
 root_attachment_id text NOT NULL,
 through_history bigint NOT NULL CHECK (through_history>=0),
 last_message_scanned bigint NOT NULL CHECK (last_message_scanned>=0),
 published_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id),
 FOREIGN KEY (organization_id,project_id,work_item_id,root_attachment_id) REFERENCES attachment_archive_references (organization_id,project_id,work_item_id,attachment_id) ON DELETE CASCADE
);
ALTER TABLE issue_storage_archives OWNER TO detent_migration;
REVOKE ALL ON issue_storage_archives FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_storage_archives TO detent_runtime;
ALTER TABLE issue_storage_archives ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_storage_archives FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON issue_storage_archives TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE issue_archive_sections ADD COLUMN kind text NOT NULL DEFAULT '',
 ADD COLUMN source_event_sequence bigint NOT NULL DEFAULT 0 CHECK (source_event_sequence>=0),
 ADD COLUMN source_conversation_sequence bigint NOT NULL DEFAULT 0 CHECK (source_conversation_sequence>=0),
 ADD COLUMN subject_id text NOT NULL DEFAULT '',
 ADD COLUMN first_sequence bigint NOT NULL DEFAULT 0 CHECK (first_sequence>=0),
 ADD COLUMN last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence>=first_sequence),
 ADD COLUMN retired_through bigint NOT NULL DEFAULT 0 CHECK (retired_through>=0 AND retired_through<=last_sequence),
 ADD COLUMN retirement_complete boolean NOT NULL DEFAULT false;

CREATE TABLE issue_archive_message_payloads (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 archive_revision bigint NOT NULL,
 section_id text NOT NULL,
 conversation_id text NOT NULL,
 message_id text NOT NULL,
 sequence bigint NOT NULL CHECK (sequence>0),
 message_revision bigint NOT NULL CHECK (message_revision>0),
 expected_reference text NOT NULL,
 content_id text NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id,archive_revision,section_id,message_id),
 FOREIGN KEY (organization_id,project_id,work_item_id,archive_revision,section_id) REFERENCES issue_archive_sections (organization_id,project_id,work_item_id,archive_revision,section_id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id,content_id) REFERENCES conversation_content_objects (organization_id,id),
 FOREIGN KEY (organization_id,conversation_id) REFERENCES conversations (organization_id,id) ON DELETE CASCADE
);
ALTER TABLE issue_archive_message_payloads OWNER TO detent_migration;
REVOKE ALL ON issue_archive_message_payloads FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_archive_message_payloads TO detent_runtime;
ALTER TABLE issue_archive_message_payloads ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_archive_message_payloads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON issue_archive_message_payloads TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE INDEX issue_archive_message_payloads_source ON issue_archive_message_payloads (organization_id,project_id,work_item_id,conversation_id,message_id,message_revision);
CREATE INDEX issue_archive_message_payloads_range ON issue_archive_message_payloads (organization_id,project_id,work_item_id,archive_revision,section_id,sequence,message_id);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE issue_archive_message_payloads;
DROP TABLE issue_storage_archives;
ALTER TABLE issue_archive_sections DROP COLUMN kind,DROP COLUMN source_event_sequence,DROP COLUMN source_conversation_sequence,DROP COLUMN subject_id,DROP COLUMN first_sequence,DROP COLUMN last_sequence,DROP COLUMN retired_through,DROP COLUMN retirement_complete;
