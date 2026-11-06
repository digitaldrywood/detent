-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS change_current_machine_reviews_idx
ON change_evidence(organization_id,change_id,version_id,review_kind,sequence DESC)
WHERE kind='review' AND NOT review_landing;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS (
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE c.oid=to_regclass('public.change_current_machine_reviews_idx') AND i.indrelid='public.change_evidence'::regclass
 AND c.relam=(SELECT oid FROM pg_am WHERE amname='btree')
 AND NOT i.indisunique AND i.indisvalid AND i.indisready AND i.indnatts=5 AND i.indnkeyatts=5
 AND pg_get_expr(i.indpred,i.indrelid)='((kind = ''review''::text) AND (NOT review_landing))'
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey::smallint[]) WITH ORDINALITY k(attnum,position)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum ORDER BY k.position)=ARRAY['organization_id','change_id','version_id','review_kind','sequence']
 AND NOT EXISTS (
  SELECT 1 FROM unnest(i.indkey::smallint[],i.indclass::oid[],i.indcollation::oid[]) k(attnum,opclass,collation_oid)
  JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
  JOIN pg_opclass o ON o.oid=k.opclass
  WHERE a.attcollation<>k.collation_oid OR NOT o.opcdefault OR o.opcintype<>a.atttypid OR o.opcnamespace<>'pg_catalog'::regnamespace
 )
 ) THEN RAISE EXCEPTION 'Current machine review index requires repair'; END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS change_current_machine_reviews_idx;
