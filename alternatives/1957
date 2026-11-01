-- +goose Up
CREATE TABLE native_archive_sections (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('run','change','revision','conversation','history')),
  subject_id TEXT NOT NULL,
  local_attempt_id INTEGER NOT NULL DEFAULT 0 CHECK(local_attempt_id >= 0),
  first_sequence INTEGER NOT NULL DEFAULT 0 CHECK(first_sequence >= 0),
  last_sequence INTEGER NOT NULL DEFAULT 0 CHECK(last_sequence >= first_sequence),
  section_id TEXT NOT NULL,
  attachment_id TEXT NOT NULL REFERENCES attachments(id),
  PRIMARY KEY(organization_id,project_id,work_item_id,kind,subject_id,first_sequence),
  UNIQUE(organization_id,project_id,work_item_id,section_id),
  FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE native_archive_sections;
