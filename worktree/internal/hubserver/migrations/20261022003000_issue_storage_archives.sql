-- +goose Up
ALTER TABLE issues ADD COLUMN terminal_entered_at TEXT;
CREATE INDEX issues_storage_archive_candidates ON issues(terminal_entered_at,id) WHERE terminal_entered_at IS NOT NULL;
CREATE TABLE issue_storage_archives (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 issue_revision INTEGER NOT NULL,
 event_sequence INTEGER NOT NULL,
 conversation_sequence INTEGER NOT NULL,
 attachment_id TEXT NOT NULL REFERENCES attachments(id),
 published_at TEXT NOT NULL,
 PRIMARY KEY(organization_id,project_id,work_item_id),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id) ON DELETE CASCADE
);
ALTER TABLE native_archive_sections ADD COLUMN retired_rows INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_archive_sections ADD COLUMN retirement_complete INTEGER NOT NULL DEFAULT 0 CHECK(retirement_complete IN (0,1));

-- +goose Down
ALTER TABLE native_archive_sections DROP COLUMN retirement_complete;
ALTER TABLE native_archive_sections DROP COLUMN retired_rows;
DROP TABLE issue_storage_archives;
DROP INDEX issues_storage_archive_candidates;
ALTER TABLE issues DROP COLUMN terminal_entered_at;
