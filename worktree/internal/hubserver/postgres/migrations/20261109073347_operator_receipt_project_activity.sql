-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout='5s';
DROP INDEX CONCURRENTLY IF EXISTS native_command_operator_project_active;
CREATE INDEX CONCURRENTLY native_command_operator_project_active
ON native_command_states(organization_id,reservation_project_id)
WHERE reservation_correlation_id IS NOT NULL AND response_claimed;
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY native_command_operator_project_active;
