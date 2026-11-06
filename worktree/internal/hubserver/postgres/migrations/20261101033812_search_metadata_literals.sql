-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION search_document_metadata() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $metadata$
DECLARE labels text[]; metadata tsvector;
BEGIN
 IF NEW.kind<>'issue' THEN RETURN NEW; END IF;
 SELECT ARRAY[p.name||'#'||i.number,i.project_id||'#'||i.number] INTO labels
 FROM public.issues i JOIN public.projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 WHERE i.organization_id=NEW.organization_id AND i.project_id=NEW.project_id AND i.native_id=NEW.work_item_id;
 WITH prefixes AS (
  SELECT DISTINCT 's'||encode(convert_to(NEW.organization_id,'UTF8'),'hex')||'z'||encode(convert_to(project,'UTF8'),'hex')||'_' AS prefix
  FROM unnest(ARRAY['',NEW.project_id]) project
 ), grams AS (
  SELECT DISTINCT encode(convert_to(substring(label FROM offset_value FOR width),'UTF8'),'hex') AS gram
  FROM unnest(labels) original(value)
  CROSS JOIN LATERAL (SELECT translate(value,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz') AS label) normalized
  CROSS JOIN generate_series(1,3) width
  CROSS JOIN LATERAL generate_series(1,length(label)-width+1) offset_value
 )
 SELECT array_to_tsvector(array_agg(prefix||'g'||gram)) INTO metadata FROM prefixes CROSS JOIN grams;
 NEW.vector:=NEW.vector||COALESCE(metadata,''::tsvector);
 RETURN NEW;
END
$metadata$;
-- +goose StatementEnd
CREATE TRIGGER search_document_metadata BEFORE INSERT OR UPDATE OF vector ON search_documents FOR EACH ROW EXECUTE FUNCTION search_document_metadata();

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
DROP TRIGGER projects_search_scope_refresh ON projects;
CREATE TRIGGER projects_search_scope_refresh AFTER UPDATE OF deleted_at,name ON projects FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();

INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id)
SELECT DISTINCT organization_id,project_id,'',false,'' FROM search_documents WHERE project_id IS NOT NULL AND kind='issue'
ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET after_id='',refresh_order=nextval('search_refresh_order');

-- +goose Down
DROP TRIGGER projects_search_scope_refresh ON projects;
CREATE TRIGGER projects_search_scope_refresh AFTER UPDATE OF deleted_at ON projects FOR EACH ROW EXECUTE FUNCTION search_parent_scope_refresh();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
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
DROP TRIGGER search_document_metadata ON search_documents;
DROP FUNCTION search_document_metadata();
