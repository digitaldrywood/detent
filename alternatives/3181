-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE native_attempts VALIDATE CONSTRAINT native_attempts_project_identity;
ALTER TABLE native_attempts ALTER COLUMN project_id SET NOT NULL;
RESET lock_timeout;

-- +goose Down
SELECT 1;
