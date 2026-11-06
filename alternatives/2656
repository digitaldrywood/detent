-- +goose NO TRANSACTION
-- +goose Up
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS change_evidence_source_identity_idx
ON change_evidence(organization_id,change_id,kind,source_key) WHERE source_key IS NOT NULL;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS (
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE c.oid=to_regclass('public.change_evidence_source_identity_idx') AND i.indrelid='public.change_evidence'::regclass
 AND c.relam=(SELECT oid FROM pg_am WHERE amname='btree')
 AND i.indisunique AND i.indisvalid AND i.indisready AND i.indnatts=4 AND i.indnkeyatts=4
 AND pg_get_expr(i.indpred,i.indrelid)='(source_key IS NOT NULL)'
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey::smallint[]) WITH ORDINALITY k(attnum,position)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum ORDER BY k.position)=ARRAY['organization_id','change_id','kind','source_key']
 AND NOT EXISTS (
  SELECT 1 FROM unnest(i.indkey::smallint[],i.indclass::oid[],i.indcollation::oid[]) k(attnum,opclass,collation_oid)
  JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
  JOIN pg_opclass o ON o.oid=k.opclass
  WHERE a.attcollation<>k.collation_oid OR NOT o.opcdefault OR o.opcintype<>a.atttypid OR o.opcnamespace<>'pg_catalog'::regnamespace
 )
 ) THEN RAISE EXCEPTION 'Change evidence source identity index requires repair'; END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE change_evidence DROP CONSTRAINT IF EXISTS change_evidence_organization_id_change_id_kind_source_key_key;
ALTER TABLE change_requests ADD COLUMN IF NOT EXISTS review_summary_sequence bigint NOT NULL DEFAULT 0 CHECK(review_summary_sequence>=0);
ALTER TABLE change_requests ADD COLUMN IF NOT EXISTS review_evidence_revision bigint NOT NULL DEFAULT 0 CHECK(review_evidence_revision>=0);
CREATE TABLE IF NOT EXISTS current_change_reviews (
 organization_id text NOT NULL,
 change_id text NOT NULL,
 version_id text NOT NULL,
 review_kind text NOT NULL CHECK(review_kind IN ('human','validator','implementation','security_audit','security_audit_failure','human_prior_approval')),
 actor_key text NOT NULL,
 decision text NOT NULL CHECK(decision IN ('approved','changes_requested')),
 evidence_sequence bigint NOT NULL,
 PRIMARY KEY(organization_id,change_id,version_id,review_kind,actor_key),
 FOREIGN KEY(organization_id,change_id,version_id) REFERENCES change_versions(organization_id,change_id,id),
 FOREIGN KEY(organization_id,change_id,evidence_sequence) REFERENCES change_evidence(organization_id,change_id,sequence)
);
ALTER TABLE current_change_reviews OWNER TO detent_migration;
REVOKE ALL ON current_change_reviews FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON current_change_reviews TO detent_runtime;
ALTER TABLE current_change_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE current_change_reviews FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_policy WHERE polrelid='public.current_change_reviews'::regclass AND polname='tenant_isolation') THEN
  CREATE POLICY tenant_isolation ON current_change_reviews TO detent_runtime,detent_migration
  USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
  WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
 END IF;
END $$;
-- +goose StatementEnd
CREATE INDEX CONCURRENTLY IF NOT EXISTS current_change_human_decisions_idx
ON current_change_reviews(organization_id,change_id,version_id,decision,evidence_sequence DESC) WHERE review_kind='human';
CREATE INDEX CONCURRENTLY IF NOT EXISTS current_change_prior_approval_idx
ON current_change_reviews(organization_id,change_id,version_id) WHERE review_kind='human_prior_approval';
CREATE INDEX CONCURRENTLY IF NOT EXISTS current_change_review_pending_idx
ON change_requests(organization_id,id) WHERE review_summary_sequence<evidence_sequence;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace AND c.relname IN ('current_change_human_decisions_idx','current_change_prior_approval_idx','current_change_review_pending_idx') AND NOT i.indisvalid
 ) THEN RAISE EXCEPTION 'Current Change review indexes require repair'; END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS change_evidence_source_identity_restore_idx
ON change_evidence(organization_id,change_id,kind,source_key);
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS (
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE c.oid=to_regclass('public.change_evidence_source_identity_restore_idx') AND i.indrelid='public.change_evidence'::regclass
 AND c.relam=(SELECT oid FROM pg_am WHERE amname='btree')
 AND i.indisunique AND i.indisvalid AND i.indisready AND i.indnatts=4 AND i.indnkeyatts=4
 AND i.indpred IS NULL AND NOT i.indnullsnotdistinct
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey::smallint[]) WITH ORDINALITY k(attnum,position)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum ORDER BY k.position)=ARRAY['organization_id','change_id','kind','source_key']
 AND NOT EXISTS (
  SELECT 1 FROM unnest(i.indkey::smallint[],i.indclass::oid[],i.indcollation::oid[]) k(attnum,opclass,collation_oid)
  JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
  JOIN pg_opclass o ON o.oid=k.opclass
  WHERE a.attcollation<>k.collation_oid OR NOT o.opcdefault OR o.opcintype<>a.atttypid OR o.opcnamespace<>'pg_catalog'::regnamespace
 )
 ) THEN RAISE EXCEPTION 'Change evidence source identity index requires repair'; END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='public.change_evidence'::regclass AND conname='change_evidence_organization_id_change_id_kind_source_key_key') THEN
 ALTER TABLE change_evidence ADD CONSTRAINT change_evidence_organization_id_change_id_kind_source_key_key UNIQUE USING INDEX change_evidence_source_identity_restore_idx;
 END IF;
END $$;
-- +goose StatementEnd
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_source_identity_idx;
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_source_identity_restore_idx;
DROP INDEX CONCURRENTLY IF EXISTS current_change_review_pending_idx;
DROP TABLE IF EXISTS current_change_reviews;
ALTER TABLE change_requests DROP COLUMN IF EXISTS review_evidence_revision;
ALTER TABLE change_requests DROP COLUMN IF EXISTS review_summary_sequence;
