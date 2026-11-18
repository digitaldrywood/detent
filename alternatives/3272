-- +goose Up
-- +goose StatementBegin
DO $roles$
BEGIN
 IF position('sd.id COLLATE "default" AS id' IN (SELECT p.prosrc FROM pg_proc p WHERE p.oid='public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[])'::regprocedure))=0 THEN
  RAISE EXCEPTION 'apply the updated roles.sql before migrating search candidates';
 END IF;
END
$roles$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
