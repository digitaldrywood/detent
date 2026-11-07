-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.mark_work_item_page_dirty() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE row_organization text; row_project text; row_item text; row_record text; row_kind text;
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  IF TG_OP='DELETE' THEN
   row_organization=OLD.organization_id; row_project=OLD.project_id; row_item=OLD.native_id;
  ELSE
   row_organization=NEW.organization_id; row_project=NEW.project_id; row_item=NEW.native_id;
  END IF;
  row_record=row_item; row_kind='item';
 ELSE
  IF TG_OP='DELETE' THEN
   row_organization=OLD.organization_id; row_project=OLD.project_id; row_item=OLD.work_item_id; row_record=OLD.id;
  ELSE
   row_organization=NEW.organization_id; row_project=NEW.project_id; row_item=NEW.work_item_id; row_record=NEW.id;
  END IF;
  row_kind='comment';
 END IF;
 INSERT INTO public.native_work_item_page_dirty (organization_id,project_id,work_item_id,record_id,kind)
 VALUES (row_organization,row_project,row_item,row_record,row_kind)
 ON CONFLICT (organization_id,project_id,work_item_id,record_id,kind)
 DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp();
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.mark_relationship_page_dirty() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE organization text; first_id bigint; second_id bigint;
BEGIN
 IF TG_TABLE_NAME='issue_dependencies' THEN
  IF TG_OP='DELETE' THEN organization=OLD.organization_id;first_id=OLD.dependent_issue_id;second_id=OLD.blocker_issue_id;
  ELSE organization=NEW.organization_id;first_id=NEW.dependent_issue_id;second_id=NEW.blocker_issue_id;END IF;
 ELSE
  IF TG_OP='DELETE' THEN organization=OLD.organization_id;first_id=OLD.first_issue_id;second_id=OLD.second_issue_id;
  ELSE organization=NEW.organization_id;first_id=NEW.first_issue_id;second_id=NEW.second_issue_id;END IF;
 END IF;
 INSERT INTO public.native_work_item_page_dirty (organization_id,project_id,work_item_id,record_id,kind)
 SELECT i.organization_id,i.project_id,i.native_id,i.native_id,'relationships' FROM public.issues i WHERE i.organization_id=organization AND i.id IN (first_id,second_id)
 ON CONFLICT (organization_id,project_id,work_item_id,record_id,kind) DO UPDATE
 SET generation=DEFAULT,source_at=clock_timestamp(),after_type='',after_direction='',after_project='',after_item='',cleaning=false;
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.mark_integration_page_dirty() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE organization text; project text;
BEGIN
 IF TG_OP='DELETE' THEN organization=OLD.organization_id; ELSE organization=NEW.organization_id; END IF;
 IF TG_TABLE_NAME='projects' THEN
  IF TG_OP='DELETE' THEN project=OLD.id; ELSE project=NEW.id; END IF;
 ELSE
  IF TG_OP='DELETE' THEN project=OLD.project_id; ELSE project=NEW.project_id; END IF;
 END IF;
 INSERT INTO public.project_integration_page_dirty(organization_id,project_id) SELECT organization,project WHERE EXISTS(SELECT 1 FROM public.projects WHERE organization_id=organization AND id=project)
 ON CONFLICT(organization_id,project_id) DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp();
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.invalidate_onboarding_page_access() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE organization text;
BEGIN
 IF TG_OP='DELETE' THEN organization=OLD.organization_id; ELSE organization=NEW.organization_id; END IF;
 INSERT INTO public.project_onboarding_page_access(organization_id) SELECT organization WHERE EXISTS(SELECT 1 FROM public.organizations WHERE organization_id=organization)
 ON CONFLICT(organization_id) DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp();
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.invalidate_health_page() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
 IF (NEW.evidence_object_key,NEW.evidence_sha256,NEW.evidence_bytes,NEW.evidence_content_bytes) IS DISTINCT FROM (OLD.evidence_object_key,OLD.evidence_sha256,OLD.evidence_bytes,OLD.evidence_content_bytes) THEN
 NEW.page_generation=OLD.page_generation+1;
 NEW.page_source_at=clock_timestamp();
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.order_health_project_page() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
 SELECT sequence INTO STRICT NEW.page_sequence FROM public.health_findings WHERE organization_id=NEW.organization_id AND id=NEW.finding_id;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.project_page_source_time() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
 IF (NEW.revision,NEW.model_revision,NEW.workflow_sha256,NEW.model_sha256) IS DISTINCT FROM (OLD.revision,OLD.model_revision,OLD.workflow_sha256,OLD.model_sha256) THEN
 NEW.page_source_at=clock_timestamp();
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.organization_model_page_source_time() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
 IF NEW.model_selection_revision IS DISTINCT FROM OLD.model_selection_revision THEN NEW.model_page_source_at=clock_timestamp(); END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'page trigger search paths require a forward repair';
END $$;
-- +goose StatementEnd
