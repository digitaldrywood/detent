-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE queue_entries
    ADD COLUMN IF NOT EXISTS list_priority integer GENERATED ALWAYS AS (coalesce(priority_override,4)) STORED,
    ADD COLUMN IF NOT EXISTS list_rank_missing integer GENERATED ALWAYS AS (CASE WHEN btrim(rank,U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')='' THEN 1 ELSE 0 END) STORED,
    ADD COLUMN IF NOT EXISTS list_rank text COLLATE "C" GENERATED ALWAYS AS (btrim(rank,U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')) STORED;
DROP INDEX CONCURRENTLY IF EXISTS queue_entries_list_keys_seek;
CREATE INDEX CONCURRENTLY queue_entries_list_keys_seek ON queue_entries (organization_id,project_id,scope,list_priority,list_rank_missing,list_rank,created_at,work_item_id);
DROP INDEX CONCURRENTLY IF EXISTS queue_entries_list_seek;
RESET lock_timeout;
-- +goose Down
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS queue_entries_list_seek ON queue_entries (organization_id,project_id,scope,(coalesce(priority_override,4)),(CASE WHEN btrim(rank)='' THEN 1 ELSE 0 END),(btrim(rank)),created_at,work_item_id);
DROP INDEX CONCURRENTLY IF EXISTS queue_entries_list_keys_seek;
ALTER TABLE queue_entries DROP COLUMN IF EXISTS list_rank,DROP COLUMN IF EXISTS list_rank_missing,DROP COLUMN IF EXISTS list_priority;
RESET lock_timeout;
