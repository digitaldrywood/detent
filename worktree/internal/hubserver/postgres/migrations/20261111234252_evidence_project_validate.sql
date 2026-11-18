-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE change_evidence VALIDATE CONSTRAINT change_evidence_project_identity;
ALTER TABLE change_evidence ALTER COLUMN project_id SET NOT NULL;
RESET lock_timeout;

-- +goose Down
SELECT 1;
