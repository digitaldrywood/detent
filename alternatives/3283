-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.mark_native_board_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE issue_probe public.issues;
BEGIN
 IF TG_OP = 'DELETE' THEN
  INSERT INTO native_board_dirty (organization_id,project_id,work_item_id) VALUES (OLD.organization_id,OLD.project_id,OLD.native_id) ON CONFLICT DO NOTHING;
  RETURN OLD;
 END IF;
 IF TG_OP = 'UPDATE' THEN
  issue_probe := OLD;
  issue_probe.source_revision := NEW.source_revision; issue_probe.source_scheduled := NEW.source_scheduled; issue_probe.source_observed_at := NEW.source_observed_at;
  IF issue_probe IS NOT DISTINCT FROM NEW THEN RETURN NEW; END IF;
 END IF;
 INSERT INTO native_board_dirty (organization_id,project_id,work_item_id) VALUES (NEW.organization_id,NEW.project_id,NEW.native_id) ON CONFLICT DO NOTHING;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.mark_native_board_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN
  INSERT INTO native_board_dirty (organization_id,project_id,work_item_id) VALUES (OLD.organization_id,OLD.project_id,OLD.native_id) ON CONFLICT DO NOTHING;
  RETURN OLD;
 END IF;
 INSERT INTO native_board_dirty (organization_id,project_id,work_item_id) VALUES (NEW.organization_id,NEW.project_id,NEW.native_id) ON CONFLICT DO NOTHING;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
