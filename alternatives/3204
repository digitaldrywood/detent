-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE change_versions VALIDATE CONSTRAINT change_versions_project_identity;
ALTER TABLE change_versions VALIDATE CONSTRAINT change_versions_landing_complete;
ALTER TABLE change_versions ALTER COLUMN project_id SET NOT NULL;
RESET lock_timeout;

-- +goose Down
SELECT 1;
