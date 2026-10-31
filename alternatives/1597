-- +goose Up
ALTER TABLE issues ADD COLUMN archived INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1));
CREATE INDEX issues_unarchived_organization ON issues (organization_id, project_id) WHERE archived = 0;

-- +goose Down
DROP INDEX issues_unarchived_organization;
ALTER TABLE issues DROP COLUMN archived;
