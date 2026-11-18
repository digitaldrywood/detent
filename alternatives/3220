-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issues_board_retention_seek;
CREATE INDEX CONCURRENTLY issues_board_retention_seek ON issues (organization_id,terminal_entered_at,id) WHERE NOT archived AND terminal_entered_at IS NOT NULL;
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS issues_board_retention_seek;
