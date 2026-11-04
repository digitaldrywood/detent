-- +goose Up
ALTER TABLE projects ADD COLUMN duplicate_source_sequence bigint NOT NULL DEFAULT 0 CHECK (duplicate_source_sequence >= 0);
CREATE INDEX search_comment_source_vector_idx ON search_documents USING gin(source_vector) WHERE kind='comment' AND NOT placeholder;

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

DROP TRIGGER issues_search_scope_refresh ON issues;
CREATE TRIGGER issues_search_scope_refresh AFTER UPDATE OF archived,actor_principal_id,revision,title,body_object_key,workflow_state_name,native_created_at ON issues FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();
DROP TRIGGER comments_search_scope_refresh ON native_comments;
CREATE TRIGGER comments_search_scope_refresh AFTER UPDATE OF deleted,revision,body_object_key ON native_comments FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'duplicate source fence requires an explicit retained-data migration'; END; $$;
-- +goose StatementEnd
