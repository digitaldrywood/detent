-- +goose Up
DROP INDEX search_postings_canonical_idx;
ALTER TABLE search_postings DROP COLUMN visible;
ALTER TABLE search_documents ADD COLUMN generation bigint NOT NULL DEFAULT 0;
ALTER TABLE search_documents ADD COLUMN source_visible boolean NOT NULL DEFAULT true;
ALTER TABLE search_documents ADD COLUMN placeholder boolean NOT NULL DEFAULT false;
ALTER TABLE search_documents ADD COLUMN aggregate_object_key text;
ALTER TABLE search_documents ADD COLUMN aggregate_sha256 text;
ALTER TABLE search_documents ADD COLUMN aggregate_bytes bigint;
ALTER TABLE search_documents ADD COLUMN aggregate_content_bytes bigint;
CREATE INDEX search_documents_sources_idx ON search_documents(organization_id,project_id,work_item_id,id COLLATE "C") WHERE kind NOT IN ('issue','thread') AND source_visible;

-- +goose StatementBegin
CREATE FUNCTION search_source_revision() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $revision$
BEGIN
 IF TG_OP='INSERT' OR NEW.revision<>OLD.revision OR NEW.payload_object_key<>OLD.payload_object_key THEN
  NEW.generation:=nextval('public.search_refresh_order');
  NEW.aggregate_object_key:=NULL; NEW.aggregate_sha256:=NULL; NEW.aggregate_bytes:=NULL; NEW.aggregate_content_bytes:=NULL;
 END IF;
 IF NEW.kind='comment' THEN SELECT NOT deleted INTO NEW.source_visible FROM public.native_comments WHERE organization_id=NEW.organization_id AND id=NEW.source_id; END IF;
 IF NEW.kind NOT IN ('issue','thread') THEN NEW.indexed_revision:=NEW.revision; NEW.vector:=''::tsvector; END IF;
 RETURN NEW;
END
$revision$;
-- +goose StatementEnd
CREATE TRIGGER search_source_revision BEFORE INSERT OR UPDATE OF revision,payload_object_key ON search_documents FOR EACH ROW EXECUTE FUNCTION search_source_revision();

-- +goose StatementBegin
CREATE FUNCTION search_work_source_changed() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $changed$
DECLARE source public.search_documents;
BEGIN
 IF TG_OP='DELETE' THEN source:=OLD; ELSE source:=NEW; END IF;
 IF source.kind='thread' THEN RETURN NULL; END IF;
 IF TG_OP<>'DELETE' AND source.kind='issue' THEN RETURN NULL; END IF;
 IF TG_OP='DELETE' AND source.kind='issue' THEN
  IF source.placeholder THEN RETURN NULL; END IF;
  SELECT * INTO source FROM public.search_documents WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id
   AND work_item_id=OLD.work_item_id AND kind NOT IN ('issue','thread') ORDER BY id COLLATE "C" LIMIT 1;
  IF NOT FOUND THEN RETURN NULL; END IF;
 END IF;
 UPDATE public.search_documents SET generation=nextval('public.search_refresh_order')
 WHERE organization_id=source.organization_id AND kind='issue' AND source_id=source.work_item_id AND indexed_revision<>revision;
 IF FOUND THEN RETURN NULL; END IF;
 INSERT INTO public.search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,title,scope,status,updated_at,updated_at_submicrosecond,
  payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,placeholder,generation)
 SELECT i.organization_id,'issue/'||i.native_id,i.project_id,i.native_id,'issue',i.native_id,1,i.title,'issue','',source.updated_at,source.updated_at_submicrosecond,
  source.payload_object_key,source.payload_sha256,source.payload_bytes,source.payload_content_bytes,true,nextval('public.search_refresh_order')
 FROM public.issues i WHERE i.organization_id=source.organization_id AND i.project_id=source.project_id AND i.native_id=source.work_item_id
 ON CONFLICT(organization_id,kind,source_id) DO UPDATE SET
  indexed_revision=0,vector=''::tsvector,source_vector=''::tsvector,generation=nextval('public.search_refresh_order'),
  aggregate_object_key=NULL,aggregate_sha256=NULL,aggregate_bytes=NULL,aggregate_content_bytes=NULL;
 RETURN NULL;
END
$changed$;
-- +goose StatementEnd
CREATE TRIGGER search_work_source_changed AFTER INSERT OR UPDATE OF revision,payload_object_key OR DELETE ON search_documents FOR EACH ROW EXECUTE FUNCTION search_work_source_changed();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
DECLARE parent_archived boolean:=false; parent_principal text:=''; hidden boolean:=false; task public.search_scope_refresh; next_id text;
BEGIN
 IF NEW.kind NOT IN ('issue','thread') THEN RETURN NEW; END IF;
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
    SELECT unnest(tsvector_to_array(NEW.source_vector)) AS term UNION SELECT unnest(tsvector_to_array(NEW.vector)) UNION SELECT lexeme FROM words
   ) terms LEFT JOIN words w ON w.lexeme=term
   UNION ALL SELECT '~all',0
   UNION ALL SELECT '~pending',0 WHERE NEW.indexed_revision<>NEW.revision
  )
  SELECT NEW.organization_id,key,l.term,NEW.id,l.impact,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key CROSS JOIN lexemes l
  WHERE NEW.indexed_revision=NEW.revision OR l.term IN ('~all','~pending')
    OR NEW.kind<>'thread' AND key=public.search_scope_key(NEW.project_id,NEW.work_item_id,'work','','','all','');
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
DECLARE project text; item text; archived_value boolean:=false; principal text:='';
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  IF NEW.archived IS NOT DISTINCT FROM OLD.archived AND NEW.actor_principal_id IS NOT DISTINCT FROM OLD.actor_principal_id THEN RETURN NEW; END IF;
  project:=NEW.project_id; item:=NEW.native_id; archived_value:=NEW.archived; principal:=COALESCE(NEW.actor_principal_id,'');
 ELSIF TG_TABLE_NAME='projects' THEN
  IF NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at AND NEW.name IS NOT DISTINCT FROM OLD.name THEN RETURN NEW; END IF;
  project:=NEW.id;item:='';
 ELSE
  IF NEW.deleted IS NOT DISTINCT FROM OLD.deleted THEN RETURN NEW; END IF;
  UPDATE public.search_documents SET source_visible=NOT NEW.deleted WHERE organization_id=NEW.organization_id AND kind='comment' AND source_id=NEW.id;
  UPDATE public.search_documents SET generation=nextval('public.search_refresh_order')
  WHERE organization_id=NEW.organization_id AND kind='issue' AND source_id=NEW.work_item_id AND indexed_revision<>revision;
  IF NOT FOUND THEN
   UPDATE public.search_documents SET generation=nextval('public.search_refresh_order'),indexed_revision=0,vector=''::tsvector,source_vector=''::tsvector,
    aggregate_object_key=NULL,aggregate_sha256=NULL,aggregate_bytes=NULL,aggregate_content_bytes=NULL
   WHERE organization_id=NEW.organization_id AND kind='issue' AND source_id=NEW.work_item_id;
  END IF;
  RETURN NEW;
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

CREATE POLICY search_work_migration ON search_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON search_postings TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON search_scope_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON issues TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON native_comments TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON projects TO detent_migration USING(true) WITH CHECK(true);
DELETE FROM search_postings p USING search_documents d WHERE p.organization_id=d.organization_id AND p.document_id=d.id AND d.kind NOT IN ('issue','thread');
DELETE FROM search_scope_documents s USING search_documents d WHERE s.organization_id=d.organization_id AND s.id=d.id AND d.kind NOT IN ('issue','thread');
UPDATE search_documents SET generation=nextval('search_refresh_order'),indexed_revision=0,vector=''::tsvector,source_vector=''::tsvector WHERE kind='issue';
UPDATE search_documents SET revision=revision,indexed_revision=revision,vector=''::tsvector WHERE kind NOT IN ('issue','thread');
DROP POLICY search_work_migration ON search_documents;
DROP POLICY search_work_migration ON search_postings;
DROP POLICY search_work_migration ON search_scope_documents;
DROP POLICY search_work_migration ON issues;
DROP POLICY search_work_migration ON native_comments;
DROP POLICY search_work_migration ON projects;

-- +goose Down
ALTER TABLE search_postings ADD COLUMN visible boolean NOT NULL DEFAULT true;
CREATE INDEX search_postings_canonical_idx ON search_postings(organization_id,scope,visible,term,impact DESC,document_id COLLATE "C");
UPDATE search_postings p SET visible=false FROM search_documents d JOIN native_comments c ON c.organization_id=d.organization_id AND c.id=d.source_id
WHERE d.kind='comment' AND c.deleted AND p.organization_id=d.organization_id AND p.document_id=d.id;
DROP TRIGGER search_work_source_changed ON search_documents;
DROP FUNCTION search_work_source_changed();
DROP TRIGGER search_source_revision ON search_documents;
DROP FUNCTION search_source_revision();
DROP INDEX search_documents_sources_idx;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
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
    SELECT unnest(tsvector_to_array(NEW.source_vector)) AS term UNION SELECT unnest(tsvector_to_array(NEW.vector)) UNION SELECT lexeme FROM words
   ) terms LEFT JOIN words w ON w.lexeme=term
   UNION ALL SELECT '~all',0
   UNION ALL SELECT '~pending',0 WHERE NEW.indexed_revision<>NEW.revision
  )
  SELECT NEW.organization_id,key,l.term,NEW.id,l.impact,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key CROSS JOIN lexemes l
  WHERE NEW.indexed_revision=NEW.revision OR l.term IN ('~all','~pending')
    OR NEW.kind<>'thread' AND key=public.search_scope_key(NEW.project_id,NEW.work_item_id,'work','','','all','');
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
DECLARE project text; item text; archived_value boolean:=false; principal text:='';
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  IF NEW.archived IS NOT DISTINCT FROM OLD.archived AND NEW.actor_principal_id IS NOT DISTINCT FROM OLD.actor_principal_id THEN RETURN NEW; END IF;
  project:=NEW.project_id; item:=NEW.native_id; archived_value:=NEW.archived; principal:=COALESCE(NEW.actor_principal_id,'');
 ELSIF TG_TABLE_NAME='projects' THEN
  IF NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at AND NEW.name IS NOT DISTINCT FROM OLD.name THEN RETURN NEW; END IF;
  project:=NEW.id;item:='';
 ELSE
  IF NEW.deleted IS NOT DISTINCT FROM OLD.deleted THEN RETURN NEW; END IF;
  UPDATE public.search_postings SET visible=NOT NEW.deleted
  WHERE organization_id=NEW.organization_id AND document_id=(
   SELECT d.id FROM public.search_documents d WHERE d.organization_id=NEW.organization_id AND d.kind='comment' AND d.source_id=NEW.id
  );
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
CREATE POLICY search_work_migration ON search_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON search_postings TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON search_scope_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON issues TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON native_comments TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_work_migration ON projects TO detent_migration USING(true) WITH CHECK(true);
DELETE FROM search_documents WHERE placeholder;
UPDATE search_documents SET indexed_revision=0,vector=''::tsvector WHERE kind<>'thread';
DROP POLICY search_work_migration ON search_documents;
DROP POLICY search_work_migration ON search_postings;
DROP POLICY search_work_migration ON search_scope_documents;
DROP POLICY search_work_migration ON issues;
DROP POLICY search_work_migration ON native_comments;
DROP POLICY search_work_migration ON projects;
ALTER TABLE search_documents DROP COLUMN generation,DROP COLUMN source_visible,DROP COLUMN placeholder,DROP COLUMN aggregate_object_key,DROP COLUMN aggregate_sha256,DROP COLUMN aggregate_bytes,DROP COLUMN aggregate_content_bytes;
