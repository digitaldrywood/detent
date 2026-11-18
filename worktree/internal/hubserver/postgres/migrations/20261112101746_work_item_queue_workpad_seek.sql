-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS queue_entries_list_seek;
CREATE INDEX CONCURRENTLY queue_entries_list_seek ON queue_entries (organization_id,project_id,scope,(coalesce(priority_override,4)),(CASE WHEN btrim(rank)='' THEN 1 ELSE 0 END),(btrim(rank)),created_at,work_item_id);
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_workpad_item;
CREATE INDEX CONCURRENTLY github_outbound_workpad_item ON github_outbound (organization_id,project_id,work_item_id,id DESC) WHERE kind='workpad' AND status<>'superseded';
RESET lock_timeout;
-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_workpad_item;
DROP INDEX CONCURRENTLY IF EXISTS queue_entries_list_seek;
RESET lock_timeout;
