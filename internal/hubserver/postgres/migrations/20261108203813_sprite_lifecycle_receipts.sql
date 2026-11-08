-- +goose Up
ALTER TABLE sprite_lifecycle_operations ADD COLUMN request_object_key text;
ALTER TABLE sprite_lifecycle_operations ADD COLUMN target_enrollment_id text NOT NULL DEFAULT '';
ALTER TABLE sprite_lifecycle_operations ADD COLUMN exec_session_id text;
ALTER TABLE sprite_lifecycle_operations ADD COLUMN provider_resource_id text NOT NULL DEFAULT '';
ALTER TABLE sprite_lifecycle_operations ADD COLUMN project_id text NOT NULL DEFAULT '';
ALTER TABLE sprite_lifecycle_operations DROP CONSTRAINT sprite_lifecycle_operations_operation_check;
ALTER TABLE sprite_lifecycle_operations ADD CONSTRAINT sprite_lifecycle_operations_operation_check CHECK (operation IN ('provision','wake','delete','bootstrap','checkpoint'));
-- +goose StatementBegin
DO $$
DECLARE membership_key text;
BEGIN
 SELECT conname INTO STRICT membership_key FROM pg_constraint
 WHERE conrelid='sprite_lifecycle_operations'::regclass AND contype='f'
 AND confrelid='runner_sprite_memberships'::regclass;
 EXECUTE format('ALTER TABLE sprite_lifecycle_operations DROP CONSTRAINT %I',membership_key);
END $$;
-- +goose StatementEnd

CREATE INDEX sprite_lifecycle_member_receipts ON sprite_lifecycle_operations(organization_id,name,created_at,idempotency_key) WHERE outcome IN ('committed','rejected') AND receipt_object_key IS NOT NULL;

CREATE INDEX sprite_lifecycle_enrollment_wakes ON sprite_lifecycle_operations(organization_id,target_enrollment_id,created_at DESC,idempotency_key DESC) WHERE operation='wake';

-- +goose Down
DROP INDEX sprite_lifecycle_enrollment_wakes;
DROP INDEX sprite_lifecycle_member_receipts;
ALTER TABLE sprite_lifecycle_operations ADD CONSTRAINT sprite_lifecycle_operations_member_fkey FOREIGN KEY (organization_id,name) REFERENCES runner_sprite_memberships(organization_id,name) NOT VALID;
ALTER TABLE sprite_lifecycle_operations DROP CONSTRAINT sprite_lifecycle_operations_operation_check;
ALTER TABLE sprite_lifecycle_operations ADD CONSTRAINT sprite_lifecycle_operations_operation_check CHECK (operation IN ('provision','wake','delete','bootstrap')) NOT VALID;
ALTER TABLE sprite_lifecycle_operations DROP COLUMN project_id;
ALTER TABLE sprite_lifecycle_operations DROP COLUMN provider_resource_id;
ALTER TABLE sprite_lifecycle_operations DROP COLUMN exec_session_id;
ALTER TABLE sprite_lifecycle_operations DROP COLUMN target_enrollment_id;
ALTER TABLE sprite_lifecycle_operations DROP COLUMN request_object_key;
