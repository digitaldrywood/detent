-- +goose Up
ALTER TABLE conversations ADD COLUMN subject_work_item_id TEXT;
CREATE INDEX conversations_subject_idx ON conversations(organization_id, project_id, subject_work_item_id, created_at DESC);

-- +goose Down
DROP INDEX conversations_subject_idx;
ALTER TABLE conversations DROP COLUMN subject_work_item_id;
