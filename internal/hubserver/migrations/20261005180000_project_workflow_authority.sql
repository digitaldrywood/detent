-- +goose Up
ALTER TABLE projects ADD COLUMN workflow_source TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN workflow_source_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN workflow_markdown TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE projects DROP COLUMN workflow_markdown;
ALTER TABLE projects DROP COLUMN workflow_source_revision;
ALTER TABLE projects DROP COLUMN workflow_source;
