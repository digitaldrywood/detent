-- +goose Up
CREATE TABLE issue_archive_sections (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  archive_revision INTEGER NOT NULL CHECK(archive_revision > 0),
  section_id TEXT NOT NULL,
  attachment_id TEXT NOT NULL REFERENCES attachments(id),
  PRIMARY KEY(organization_id,project_id,work_item_id,archive_revision,section_id),
  FOREIGN KEY(organization_id,project_id,work_item_id,attachment_id)
    REFERENCES attachment_archive_references(organization_id,project_id,work_item_id,attachment_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE issue_archive_sections;
