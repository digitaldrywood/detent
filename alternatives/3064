-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_source_revision() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $revision$
BEGIN
 IF TG_OP='UPDATE' AND OLD.search_format>0
  AND (NEW.content_revision,NEW.content_sha256,NEW.payload_object_key)
   IS NOT DISTINCT FROM (OLD.content_revision,OLD.content_sha256,OLD.payload_object_key) THEN RETURN NEW; END IF;
 IF TG_OP='INSERT' OR NEW.revision<>OLD.revision OR NEW.payload_object_key<>OLD.payload_object_key
  OR (NEW.content_revision,NEW.content_sha256) IS DISTINCT FROM (OLD.content_revision,OLD.content_sha256) THEN
  NEW.generation:=nextval('public.search_refresh_order');
  NEW.aggregate_object_key:=NULL; NEW.aggregate_sha256:=NULL; NEW.aggregate_bytes:=NULL; NEW.aggregate_content_bytes:=NULL;
 END IF;
 IF NEW.kind='comment' THEN SELECT NOT deleted INTO NEW.source_visible FROM public.native_comments WHERE organization_id=NEW.organization_id AND id=NEW.source_id; END IF;
 IF NEW.kind NOT IN ('issue','thread') THEN NEW.indexed_revision:=NEW.revision; NEW.vector:=''::tsvector; END IF;
 RETURN NEW;
END
$revision$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_work_source_changed() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $changed$
DECLARE source public.search_documents;
BEGIN
 IF TG_OP='UPDATE' AND OLD.search_format>0
  AND (NEW.content_revision,NEW.content_sha256,NEW.payload_object_key,NEW.source_visible)
   IS NOT DISTINCT FROM (OLD.content_revision,OLD.content_sha256,OLD.payload_object_key,OLD.source_visible) THEN RETURN NULL; END IF;
 IF TG_OP='DELETE' THEN source:=OLD; ELSE source:=NEW; END IF;
 IF source.kind='thread' THEN RETURN NULL; END IF;
 UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=source.organization_id AND id=source.project_id;
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
DECLARE project text; item text; archived_value boolean:=false; principal text:='';
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
  IF NEW.revision<>OLD.revision OR NEW.title IS DISTINCT FROM OLD.title OR NEW.body_object_key IS DISTINCT FROM OLD.body_object_key THEN
   UPDATE public.search_documents SET indexed_revision=0,vector=''::tsvector,generation=nextval('public.search_refresh_order')
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND kind='issue' AND source_id=NEW.native_id
    AND (search_format=0 OR NEW.title IS DISTINCT FROM OLD.title OR NEW.body_object_key IS DISTINCT FROM OLD.body_object_key);
  END IF;
  IF NEW.archived IS NOT DISTINCT FROM OLD.archived AND NEW.actor_principal_id IS NOT DISTINCT FROM OLD.actor_principal_id THEN RETURN NEW; END IF;
  project:=NEW.project_id; item:=NEW.native_id; archived_value:=NEW.archived; principal:=COALESCE(NEW.actor_principal_id,'');
 ELSIF TG_TABLE_NAME='projects' THEN
  IF NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at AND NEW.name IS NOT DISTINCT FROM OLD.name THEN RETURN NEW; END IF;
  project:=NEW.id;item:='';
 ELSE
  UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
  IF NEW.revision IS DISTINCT FROM OLD.revision OR NEW.body_object_key IS DISTINCT FROM OLD.body_object_key THEN
   UPDATE public.search_documents SET indexed_revision=0,vector=''::tsvector,generation=nextval('public.search_refresh_order')
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND kind='issue' AND source_id=NEW.work_item_id;
  END IF;
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
 IF (item='' AND EXISTS(SELECT 1 FROM public.search_documents d WHERE d.search_format=0 AND d.organization_id=NEW.organization_id AND d.project_id=project))
  OR (item<>'' AND EXISTS(SELECT 1 FROM public.search_documents d WHERE d.search_format=0 AND d.organization_id=NEW.organization_id AND d.project_id=project AND d.work_item_id=item)) THEN
  INSERT INTO public.search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id)
  VALUES(NEW.organization_id,project,item,archived_value,principal)
  ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET archived=EXCLUDED.archived,principal_id=EXCLUDED.principal_id,after_id='',refresh_order=nextval('public.search_refresh_order');
 END IF;
 RETURN NEW;
END
$refresh$;
-- +goose StatementEnd
DROP TRIGGER search_document_postings ON search_documents;
CREATE TRIGGER search_document_postings AFTER INSERT OR UPDATE OF revision,indexed_revision,vector,title,scope,status,updated_at,updated_at_submicrosecond ON search_documents FOR EACH ROW WHEN (NEW.search_format=0) EXECUTE FUNCTION search_document_postings();
DROP TRIGGER search_source_revision ON search_documents;
CREATE TRIGGER search_source_revision BEFORE INSERT OR UPDATE OF revision,payload_object_key,content_revision,content_sha256 ON search_documents FOR EACH ROW EXECUTE FUNCTION search_source_revision();
DROP TRIGGER search_work_source_changed ON search_documents;
CREATE TRIGGER search_work_source_changed AFTER INSERT OR UPDATE OF revision,payload_object_key,content_revision,content_sha256,source_visible OR DELETE ON search_documents FOR EACH ROW EXECUTE FUNCTION search_work_source_changed();

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_source_revision() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $revision$
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_work_source_changed() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $changed$
DECLARE source public.search_documents;
BEGIN
 IF TG_OP='DELETE' THEN source:=OLD; ELSE source:=NEW; END IF;
 IF source.kind='thread' THEN RETURN NULL; END IF;
 UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=source.organization_id AND id=source.project_id;
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
DECLARE project text; item text; archived_value boolean:=false; principal text:='';
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
  IF NEW.revision<>OLD.revision OR NEW.title IS DISTINCT FROM OLD.title OR NEW.body_object_key IS DISTINCT FROM OLD.body_object_key THEN
   UPDATE public.search_documents SET indexed_revision=0,vector=''::tsvector,generation=nextval('public.search_refresh_order')
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND kind='issue' AND source_id=NEW.native_id;
  END IF;
  IF NEW.archived IS NOT DISTINCT FROM OLD.archived AND NEW.actor_principal_id IS NOT DISTINCT FROM OLD.actor_principal_id THEN RETURN NEW; END IF;
  project:=NEW.project_id; item:=NEW.native_id; archived_value:=NEW.archived; principal:=COALESCE(NEW.actor_principal_id,'');
 ELSIF TG_TABLE_NAME='projects' THEN
  IF NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at AND NEW.name IS NOT DISTINCT FROM OLD.name THEN RETURN NEW; END IF;
  project:=NEW.id;item:='';
 ELSE
  UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
  IF NEW.revision IS DISTINCT FROM OLD.revision OR NEW.body_object_key IS DISTINCT FROM OLD.body_object_key THEN
   UPDATE public.search_documents SET indexed_revision=0,vector=''::tsvector,generation=nextval('public.search_refresh_order')
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND kind='issue' AND source_id=NEW.work_item_id;
  END IF;
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
DROP TRIGGER search_document_postings ON search_documents;
CREATE TRIGGER search_document_postings AFTER INSERT OR UPDATE OF revision,indexed_revision,vector,title,scope,status,updated_at,updated_at_submicrosecond ON search_documents FOR EACH ROW EXECUTE FUNCTION search_document_postings();
DROP TRIGGER search_source_revision ON search_documents;
CREATE TRIGGER search_source_revision BEFORE INSERT OR UPDATE OF revision,payload_object_key ON search_documents FOR EACH ROW EXECUTE FUNCTION search_source_revision();
DROP TRIGGER search_work_source_changed ON search_documents;
CREATE TRIGGER search_work_source_changed AFTER INSERT OR UPDATE OF revision,payload_object_key OR DELETE ON search_documents FOR EACH ROW EXECUTE FUNCTION search_work_source_changed();
