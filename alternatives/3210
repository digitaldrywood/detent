-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_current_object_key_nonnull;
CREATE INDEX CONCURRENTLY native_board_deltas_current_object_key_nonnull ON native_board_deltas (organization_id,current_object_key) WHERE current_object_key IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_current_object_key_lookup;
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_previous_object_key_nonnull;
CREATE INDEX CONCURRENTLY native_board_deltas_previous_object_key_nonnull ON native_board_deltas (organization_id,previous_object_key) WHERE previous_object_key IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_previous_object_key_lookup;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_current_object_key_lookup;
CREATE INDEX CONCURRENTLY native_board_deltas_current_object_key_lookup ON native_board_deltas (organization_id,current_object_key);
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_current_object_key_nonnull;
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_previous_object_key_lookup;
CREATE INDEX CONCURRENTLY native_board_deltas_previous_object_key_lookup ON native_board_deltas (organization_id,previous_object_key);
DROP INDEX CONCURRENTLY IF EXISTS native_board_deltas_previous_object_key_nonnull;
RESET lock_timeout;
