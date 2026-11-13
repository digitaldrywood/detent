-- +goose Up
DROP TABLE search_postings;
CREATE TABLE search_postings(
 organization_id text NOT NULL,scope text NOT NULL,term text COLLATE "C" NOT NULL,document_id text NOT NULL,
 principal_id text NOT NULL DEFAULT '',thread boolean NOT NULL DEFAULT false,
 impact double precision NOT NULL,updated_at timestamptz NOT NULL,updated_at_submicrosecond bigint NOT NULL,
 literal_positions bigint[] NOT NULL DEFAULT '{}',
 PRIMARY KEY(organization_id,document_id,term,scope),
 FOREIGN KEY(organization_id,document_id) REFERENCES search_documents(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX search_postings_impact_idx ON search_postings(organization_id,scope,term,impact DESC,document_id COLLATE "C");
CREATE INDEX search_postings_reporter_idx ON search_postings(organization_id,scope,principal_id,term,impact DESC,document_id COLLATE "C");
CREATE INDEX search_postings_time_idx ON search_postings(organization_id,scope,term,updated_at DESC,updated_at_submicrosecond DESC,document_id COLLATE "C" DESC) WHERE thread;
ALTER TABLE search_postings ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_postings FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_postings TO detent_runtime;
GRANT SELECT ON search_postings TO detent_platform;
CREATE POLICY tenant_isolation ON search_postings TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON search_postings FOR SELECT TO detent_platform USING (true);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
DECLARE parent_archived boolean:=false; parent_principal text:=''; hidden boolean:=false; task public.search_scope_refresh; next_id text; visibility_scopes text[];
BEGIN
 IF NEW.kind NOT IN ('issue','thread') THEN RETURN NEW; END IF;
 IF NEW.project_id IS NOT NULL THEN
  INSERT INTO public.search_scope_documents(organization_id,project_id,work_item_id,id)
   SELECT DISTINCT NEW.organization_id,NEW.project_id,item,NEW.id FROM unnest(ARRAY['',COALESCE(NEW.work_item_id,'')]) item ON CONFLICT DO NOTHING;
 END IF;
 IF NEW.project_id IS NOT NULL THEN
  SELECT deleted_at IS NOT NULL INTO hidden FROM public.projects WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 END IF;
 IF NEW.work_item_id IS NOT NULL THEN
  SELECT archived,COALESCE(actor_principal_id,'') INTO parent_archived,parent_principal FROM public.issues
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND native_id=NEW.work_item_id;
 END IF;
 IF NOT hidden THEN
  SELECT array_agg(key) INTO visibility_scopes
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key;
 END IF;
 DELETE FROM public.search_postings
 WHERE organization_id=NEW.organization_id AND document_id=NEW.id AND term IN ('~all','~pending')
  AND (hidden OR NOT (scope=ANY(visibility_scopes)) OR (NEW.indexed_revision=NEW.revision AND term='~pending'));
 IF NOT hidden THEN
  INSERT INTO public.search_postings(organization_id,scope,term,document_id,principal_id,thread,impact,updated_at,updated_at_submicrosecond)
  SELECT NEW.organization_id,key,term,NEW.id,CASE WHEN NEW.kind='thread' THEN '' ELSE COALESCE(parent_principal,'') END,NEW.kind='thread',0,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM unnest(visibility_scopes) key
  CROSS JOIN unnest(CASE WHEN NEW.indexed_revision=NEW.revision THEN ARRAY['~all'] ELSE ARRAY['~all','~pending'] END) term
  ON CONFLICT(organization_id,scope,term,document_id) DO UPDATE
  SET impact=EXCLUDED.impact,updated_at=EXCLUDED.updated_at,updated_at_submicrosecond=EXCLUDED.updated_at_submicrosecond,principal_id=EXCLUDED.principal_id
  WHERE (search_postings.impact,search_postings.updated_at,search_postings.updated_at_submicrosecond,search_postings.principal_id)
   IS DISTINCT FROM (EXCLUDED.impact,EXCLUDED.updated_at,EXCLUDED.updated_at_submicrosecond,EXCLUDED.principal_id);
 END IF;
 IF NEW.indexed_revision=NEW.revision OR (TG_OP='UPDATE' AND NEW.refresh_order<>OLD.refresh_order AND NEW.indexed_revision=0) THEN
  FOR task IN SELECT q.* FROM public.search_scope_refresh q WHERE q.organization_id=NEW.organization_id AND q.project_id=NEW.project_id AND q.work_item_id IN ('',COALESCE(NEW.work_item_id,'')) FOR UPDATE LOOP
   IF task.work_item_id='' THEN
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   ELSE
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   END IF;
   IF next_id=NEW.id THEN
    UPDATE public.search_scope_refresh SET after_id=NEW.id,refresh_order=nextval('public.search_refresh_order') WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    IF task.work_item_id='' THEN
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    ELSE
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    END IF;
    IF next_id IS NULL THEN
     DELETE FROM public.search_scope_refresh WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    END IF;
   END IF;
  END LOOP;
 END IF;
 RETURN NEW;
END
$postings$;
-- +goose StatementEnd

CREATE POLICY search_single_scope_migration ON search_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_single_scope_migration ON search_postings TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_single_scope_migration ON search_scope_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_single_scope_migration ON search_scope_refresh TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_single_scope_migration ON issues TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_single_scope_migration ON projects TO detent_migration USING(true) WITH CHECK(true);
-- +goose StatementBegin
DO $roles$
BEGIN
 IF (SELECT count(*) FROM public.search_scope_keys('example','example','issue','issue','',false,'example'))<>1 THEN
  RAISE EXCEPTION 'apply the updated roles.sql before migrating search postings';
 END IF;
END
$roles$;
-- +goose StatementEnd
INSERT INTO search_postings(organization_id,scope,term,document_id,principal_id,thread,impact,updated_at,updated_at_submicrosecond)
SELECT d.organization_id,k.key,m.term,d.id,CASE WHEN d.kind='thread' THEN '' ELSE COALESCE(i.actor_principal_id,'') END,d.kind='thread',0,d.updated_at,d.updated_at_submicrosecond
FROM search_documents d
LEFT JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id
CROSS JOIN LATERAL public.search_scope_keys(d.project_id,d.work_item_id,d.kind,d.scope,d.status,COALESCE(i.archived,false),COALESCE(i.actor_principal_id,'')) k(key)
CROSS JOIN (VALUES ('~all'),('~pending')) m(term)
WHERE d.kind IN ('issue','thread')
 AND (d.project_id IS NULL OR EXISTS(SELECT 1 FROM projects p WHERE p.organization_id=d.organization_id AND p.id=d.project_id AND p.deleted_at IS NULL))
ON CONFLICT DO NOTHING;
INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id)
SELECT DISTINCT d.organization_id,d.project_id,'',false,'' FROM search_documents d WHERE d.project_id IS NOT NULL AND d.kind IN ('issue','thread')
 AND EXISTS(SELECT 1 FROM projects p WHERE p.organization_id=d.organization_id AND p.id=d.project_id)
ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET after_id='',refresh_order=nextval('search_refresh_order');
UPDATE search_documents SET refresh_order=nextval('search_refresh_order'),indexed_revision=0,vector=''::tsvector
WHERE project_id IS NULL AND kind='thread' AND indexed_revision=revision;
DROP POLICY search_single_scope_migration ON search_documents;
DROP POLICY search_single_scope_migration ON search_postings;
DROP POLICY search_single_scope_migration ON search_scope_documents;
DROP POLICY search_single_scope_migration ON search_scope_refresh;
DROP POLICY search_single_scope_migration ON issues;
DROP POLICY search_single_scope_migration ON projects;

-- +goose Down
DROP TABLE search_postings;
CREATE TABLE search_postings(
 organization_id text NOT NULL,scope text NOT NULL,term text COLLATE "C" NOT NULL,document_id text NOT NULL,
 impact double precision NOT NULL,updated_at timestamptz NOT NULL,updated_at_submicrosecond bigint NOT NULL,
 literal_positions bigint[] NOT NULL DEFAULT '{}',
 PRIMARY KEY(organization_id,scope,term,document_id),
 FOREIGN KEY(organization_id,document_id) REFERENCES search_documents(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX search_postings_document_idx ON search_postings(organization_id,document_id,term,scope);
CREATE INDEX search_postings_impact_idx ON search_postings(organization_id,scope,term,impact DESC,document_id COLLATE "C");
CREATE INDEX search_postings_time_idx ON search_postings(organization_id,scope,term,updated_at DESC,updated_at_submicrosecond DESC,document_id COLLATE "C" DESC);
ALTER TABLE search_postings ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_postings FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_postings TO detent_runtime;
GRANT SELECT ON search_postings TO detent_platform;
CREATE POLICY tenant_isolation ON search_postings TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON search_postings FOR SELECT TO detent_platform USING (true);
CREATE POLICY search_single_scope_rollback ON search_scope_refresh TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_single_scope_rollback ON search_documents TO detent_migration USING(true) WITH CHECK(true);
INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id)
SELECT DISTINCT d.organization_id,d.project_id,'',false,'' FROM search_documents d WHERE d.project_id IS NOT NULL AND d.kind IN ('issue','thread')
 AND EXISTS(SELECT 1 FROM projects p WHERE p.organization_id=d.organization_id AND p.id=d.project_id)
ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET after_id='',refresh_order=nextval('search_refresh_order');
DROP POLICY search_single_scope_rollback ON search_scope_refresh;
DROP POLICY search_single_scope_rollback ON search_documents;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
DECLARE parent_archived boolean:=false; parent_principal text:=''; hidden boolean:=false; task public.search_scope_refresh; next_id text; visibility_scopes text[];
BEGIN
 IF NEW.kind NOT IN ('issue','thread') THEN RETURN NEW; END IF;
 IF NEW.project_id IS NOT NULL THEN
  INSERT INTO public.search_scope_documents(organization_id,project_id,work_item_id,id)
   SELECT DISTINCT NEW.organization_id,NEW.project_id,item,NEW.id FROM unnest(ARRAY['',COALESCE(NEW.work_item_id,'')]) item ON CONFLICT DO NOTHING;
 END IF;
 IF NEW.project_id IS NOT NULL THEN
  SELECT deleted_at IS NOT NULL INTO hidden FROM public.projects WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 END IF;
 IF NEW.work_item_id IS NOT NULL THEN
  SELECT archived,COALESCE(actor_principal_id,'') INTO parent_archived,parent_principal FROM public.issues
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND native_id=NEW.work_item_id;
 END IF;
 IF NOT hidden THEN
  SELECT array_agg(key) INTO visibility_scopes
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key;
 END IF;
 DELETE FROM public.search_postings
 WHERE organization_id=NEW.organization_id AND document_id=NEW.id AND term IN ('~all','~pending')
  AND (hidden OR NOT (scope=ANY(visibility_scopes)) OR (NEW.indexed_revision=NEW.revision AND term='~pending'));
 IF NOT hidden THEN
  INSERT INTO public.search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
  SELECT NEW.organization_id,key,term,NEW.id,0,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM unnest(visibility_scopes) key
  CROSS JOIN unnest(CASE WHEN NEW.indexed_revision=NEW.revision THEN ARRAY['~all'] ELSE ARRAY['~all','~pending'] END) term
  ON CONFLICT(organization_id,scope,term,document_id) DO UPDATE
  SET impact=EXCLUDED.impact,updated_at=EXCLUDED.updated_at,updated_at_submicrosecond=EXCLUDED.updated_at_submicrosecond
  WHERE (search_postings.impact,search_postings.updated_at,search_postings.updated_at_submicrosecond)
   IS DISTINCT FROM (EXCLUDED.impact,EXCLUDED.updated_at,EXCLUDED.updated_at_submicrosecond);
 END IF;
 IF NEW.indexed_revision=NEW.revision OR (TG_OP='UPDATE' AND NEW.refresh_order<>OLD.refresh_order AND NEW.indexed_revision=0) THEN
  FOR task IN SELECT q.* FROM public.search_scope_refresh q WHERE q.organization_id=NEW.organization_id AND q.project_id=NEW.project_id AND q.work_item_id IN ('',COALESCE(NEW.work_item_id,'')) FOR UPDATE LOOP
   IF task.work_item_id='' THEN
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   ELSE
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   END IF;
   IF next_id=NEW.id THEN
    UPDATE public.search_scope_refresh SET after_id=NEW.id,refresh_order=nextval('public.search_refresh_order') WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    IF task.work_item_id='' THEN
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    ELSE
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    END IF;
    IF next_id IS NULL THEN
     DELETE FROM public.search_scope_refresh WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    END IF;
   END IF;
  END LOOP;
 END IF;
 RETURN NEW;
END
$postings$;
-- +goose StatementEnd
