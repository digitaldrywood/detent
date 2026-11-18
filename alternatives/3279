-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.mark_work_item_page_dirty() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE row_organization text; row_project text; row_item text; row_record text; row_kind text; issue_probe public.issues; comment_probe public.native_comments;
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  IF TG_OP='UPDATE' THEN
   issue_probe := OLD;
   issue_probe.source_revision := NEW.source_revision; issue_probe.source_scheduled := NEW.source_scheduled; issue_probe.source_observed_at := NEW.source_observed_at;
   IF issue_probe IS NOT DISTINCT FROM NEW THEN RETURN NULL; END IF;
  END IF;
  IF TG_OP='DELETE' THEN
   row_organization=OLD.organization_id; row_project=OLD.project_id; row_item=OLD.native_id;
  ELSE
   row_organization=NEW.organization_id; row_project=NEW.project_id; row_item=NEW.native_id;
  END IF;
  row_record=row_item; row_kind='item';
 ELSE
  IF TG_OP='UPDATE' THEN
   comment_probe := OLD;
   comment_probe.source_revision := NEW.source_revision; comment_probe.source_scheduled := NEW.source_scheduled; comment_probe.source_observed_at := NEW.source_observed_at;
   IF comment_probe IS NOT DISTINCT FROM NEW THEN RETURN NULL; END IF;
  END IF;
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

-- +goose Down
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
