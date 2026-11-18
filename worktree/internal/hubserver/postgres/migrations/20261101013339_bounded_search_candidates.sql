-- +goose Up
CREATE SEQUENCE search_refresh_order;
GRANT USAGE ON SEQUENCE search_refresh_order TO detent_runtime;
ALTER TABLE search_documents ADD COLUMN refresh_order bigint NOT NULL DEFAULT 0;
DROP INDEX search_documents_pending_idx;
CREATE INDEX search_documents_pending_idx ON search_documents(organization_id,refresh_order,id) WHERE indexed_revision<>revision;
CREATE INDEX search_documents_scope_parent_idx ON search_documents(organization_id,project_id,id COLLATE "C");
CREATE INDEX search_documents_item_parent_idx ON search_documents(organization_id,project_id,work_item_id,id COLLATE "C");

CREATE TABLE search_postings(
 organization_id text NOT NULL,scope text NOT NULL,term text COLLATE "C" NOT NULL,document_id text NOT NULL,
 impact double precision NOT NULL,updated_at timestamptz NOT NULL,updated_at_submicrosecond bigint NOT NULL,
 PRIMARY KEY(organization_id,scope,term,document_id),
 FOREIGN KEY(organization_id,document_id) REFERENCES search_documents(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX search_postings_document_idx ON search_postings(organization_id,document_id,term,scope);
CREATE INDEX search_postings_impact_idx ON search_postings(organization_id,scope,term,impact DESC,document_id COLLATE "C");
CREATE INDEX search_postings_time_idx ON search_postings(organization_id,scope,term,updated_at DESC,updated_at_submicrosecond DESC,document_id COLLATE "C" DESC);
CREATE TABLE search_scope_documents(
 organization_id text NOT NULL,project_id text NOT NULL,work_item_id text NOT NULL,id text COLLATE "C" NOT NULL,
 PRIMARY KEY(organization_id,project_id,work_item_id,id)
);
ALTER TABLE search_scope_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_scope_documents FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_scope_documents TO detent_runtime;
GRANT SELECT ON search_scope_documents TO detent_platform;
CREATE POLICY tenant_isolation ON search_scope_documents TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON search_scope_documents FOR SELECT TO detent_platform USING (true);
CREATE TABLE search_scope_refresh(
 organization_id text NOT NULL,project_id text NOT NULL,work_item_id text NOT NULL,
 archived boolean NOT NULL,principal_id text NOT NULL,after_id text NOT NULL DEFAULT '',refresh_order bigint NOT NULL DEFAULT nextval('search_refresh_order'),
 PRIMARY KEY(organization_id,project_id,work_item_id),FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX search_scope_refresh_item_idx ON search_scope_refresh(organization_id,work_item_id) WHERE work_item_id<>'';
CREATE INDEX search_scope_refresh_turn_idx ON search_scope_refresh(organization_id,refresh_order);
CREATE INDEX search_scope_refresh_archive_idx ON search_scope_refresh(organization_id,archived,refresh_order);
CREATE INDEX search_scope_refresh_project_idx ON search_scope_refresh(organization_id,project_id,archived,refresh_order);
CREATE INDEX search_scope_refresh_reporter_idx ON search_scope_refresh(organization_id,project_id,principal_id,archived,refresh_order);

ALTER TABLE search_postings ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_postings FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_postings TO detent_runtime;
GRANT SELECT ON search_postings TO detent_platform;
CREATE POLICY tenant_isolation ON search_postings TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON search_postings FOR SELECT TO detent_platform USING (true);
ALTER TABLE search_scope_refresh ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_scope_refresh FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_scope_refresh TO detent_runtime;
GRANT SELECT ON search_scope_refresh TO detent_platform;
CREATE POLICY tenant_isolation ON search_scope_refresh TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON search_scope_refresh FOR SELECT TO detent_platform USING (true);

-- +goose StatementBegin
CREATE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
DECLARE parent_archived boolean:=false; parent_principal text:=''; hidden boolean:=false; task public.search_scope_refresh; next_id text;
BEGIN
 IF NEW.project_id IS NOT NULL THEN
  INSERT INTO public.search_scope_documents(organization_id,project_id,work_item_id,id)
   SELECT DISTINCT NEW.organization_id,NEW.project_id,item,NEW.id FROM unnest(ARRAY['',COALESCE(NEW.work_item_id,'')]) item ON CONFLICT DO NOTHING;
 END IF;
 DELETE FROM public.search_postings WHERE organization_id=NEW.organization_id AND document_id=NEW.id;
 IF NEW.project_id IS NOT NULL THEN
  SELECT deleted_at IS NOT NULL INTO hidden FROM public.projects WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 END IF;
 IF NEW.work_item_id IS NOT NULL THEN
  SELECT archived,COALESCE(actor_principal_id,'') INTO parent_archived,parent_principal FROM public.issues
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND native_id=NEW.work_item_id;
 END IF;
 IF NEW.kind='comment' THEN
  SELECT hidden OR deleted INTO hidden FROM public.native_comments WHERE organization_id=NEW.organization_id AND id=NEW.source_id;
 END IF;
 IF NOT hidden THEN
  INSERT INTO public.search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
  WITH prefixes AS (
   SELECT DISTINCT 's'||encode(convert_to(NEW.organization_id,'UTF8'),'hex')||'z'||encode(convert_to(p,'UTF8'),'hex')||'_' AS prefix
   FROM unnest(ARRAY['',COALESCE(NEW.project_id,'')]) p
  ), words AS (
   SELECT prefix||'w'||term AS lexeme,frequency::double precision/(1+NEW.token_count) AS impact
   FROM prefixes CROSS JOIN unnest(NEW.terms,NEW.term_frequencies) AS t(term,frequency)
  ), lexemes AS (
   SELECT term,COALESCE(w.impact,0) AS impact FROM (
    SELECT unnest(tsvector_to_array(NEW.vector)) AS term UNION SELECT lexeme FROM words
   ) terms LEFT JOIN words w ON w.lexeme=term
   UNION ALL SELECT '~all',0
   UNION ALL SELECT '~pending',0 WHERE NEW.indexed_revision<>NEW.revision
  )
  SELECT NEW.organization_id,key,l.term,NEW.id,l.impact,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key CROSS JOIN lexemes l;
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
CREATE TRIGGER search_document_postings AFTER INSERT OR UPDATE OF revision,indexed_revision,vector,title,scope,status,updated_at,updated_at_submicrosecond ON search_documents FOR EACH ROW EXECUTE FUNCTION search_document_postings();

-- +goose StatementBegin
CREATE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
DECLARE project text; item text; archived_value boolean:=false; principal text:='';
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  IF NEW.archived IS NOT DISTINCT FROM OLD.archived AND NEW.actor_principal_id IS NOT DISTINCT FROM OLD.actor_principal_id THEN RETURN NEW; END IF;
  project:=NEW.project_id; item:=NEW.native_id; archived_value:=NEW.archived; principal:=COALESCE(NEW.actor_principal_id,'');
 ELSIF TG_TABLE_NAME='projects' THEN
  IF NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at THEN RETURN NEW; END IF;
  project:=NEW.id;item:='';
 ELSE
  IF NEW.deleted IS NOT DISTINCT FROM OLD.deleted THEN RETURN NEW; END IF;
  project:=NEW.project_id;item:=NEW.work_item_id;
  SELECT archived,COALESCE(actor_principal_id,'') INTO archived_value,principal FROM public.issues WHERE organization_id=NEW.organization_id AND project_id=project AND native_id=item;
 END IF;
 IF (item='' AND EXISTS(SELECT 1 FROM public.search_documents d WHERE d.organization_id=NEW.organization_id AND d.project_id=project))
  OR (item<>'' AND EXISTS(SELECT 1 FROM public.search_documents d WHERE d.organization_id=NEW.organization_id AND d.project_id=project AND d.work_item_id=item)) THEN
  INSERT INTO public.search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id)
  VALUES(NEW.organization_id,project,item,archived_value,principal)
  ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET archived=EXCLUDED.archived,principal_id=EXCLUDED.principal_id,after_id='',refresh_order=nextval('public.search_refresh_order');
 END IF;
 RETURN NEW;
END
$refresh$;
-- +goose StatementEnd
CREATE TRIGGER issues_search_scope_refresh AFTER UPDATE OF archived,actor_principal_id ON issues FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();
CREATE TRIGGER projects_search_scope_refresh AFTER UPDATE OF deleted_at ON projects FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();
CREATE TRIGGER comments_search_scope_refresh AFTER UPDATE OF deleted ON native_comments FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();


-- +goose StatementBegin
CREATE FUNCTION search_deleted_scope_cleanup() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $cleanup$
DECLARE task public.search_scope_refresh; next_id text;
BEGIN
 DELETE FROM public.search_scope_documents WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id AND work_item_id IN ('',COALESCE(OLD.work_item_id,'')) AND id=OLD.id;
 FOR task IN SELECT q.* FROM public.search_scope_refresh q WHERE q.organization_id=OLD.organization_id AND q.project_id=OLD.project_id AND q.work_item_id IN ('',COALESCE(OLD.work_item_id,'')) FOR UPDATE LOOP
  IF task.work_item_id='' THEN
   SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
  ELSE
   SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
  END IF;
  IF next_id IS NULL THEN DELETE FROM public.search_scope_refresh WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id; END IF;
 END LOOP;
 RETURN OLD;
END
$cleanup$;
-- +goose StatementEnd
CREATE TRIGGER search_deleted_scope_cleanup AFTER DELETE ON search_documents FOR EACH ROW EXECUTE FUNCTION search_deleted_scope_cleanup();

-- +goose Down
DROP TRIGGER search_deleted_scope_cleanup ON search_documents;
DROP FUNCTION search_deleted_scope_cleanup();
DROP TRIGGER comments_search_scope_refresh ON native_comments;
DROP TRIGGER projects_search_scope_refresh ON projects;
DROP TRIGGER issues_search_scope_refresh ON issues;
DROP FUNCTION search_parent_scope_refresh();
DROP TRIGGER search_document_postings ON search_documents;
DROP FUNCTION search_document_postings();
DROP TABLE search_scope_refresh;
DROP TABLE search_scope_documents;
DROP TABLE search_postings;
DROP INDEX search_documents_scope_parent_idx;
DROP INDEX search_documents_item_parent_idx;
DROP INDEX search_documents_pending_idx;
CREATE INDEX search_documents_pending_idx ON search_documents(organization_id,id) WHERE indexed_revision<>revision;
ALTER TABLE search_documents DROP COLUMN refresh_order;
DROP SEQUENCE search_refresh_order;
