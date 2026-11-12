-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts ADD COLUMN project_id text;
ALTER TABLE native_attempts ADD CONSTRAINT native_attempts_project_identity FOREIGN KEY (organization_id,project_id,issue_id) REFERENCES work_item_identities (organization_id,project_id,id) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts DROP CONSTRAINT native_attempts_project_identity;
ALTER TABLE native_attempts DROP COLUMN project_id;
