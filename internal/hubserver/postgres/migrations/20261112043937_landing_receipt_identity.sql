-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE change_landing_receipts ADD COLUMN issue_id bigint;
ALTER TABLE change_landing_receipts ADD COLUMN change_id text;
ALTER TABLE change_landing_receipts ADD COLUMN fencing_token bigint;
ALTER TABLE change_landing_receipts ADD COLUMN repository_identity text;
ALTER TABLE change_landing_receipts ADD COLUMN project_id text;
-- +goose StatementBegin
CREATE FUNCTION authorize_landing_receipt_initialization() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE unchanged record; expected record;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT a.issue_id,a.fencing_token,v.change_id,c.project_id,regexp_replace(regexp_replace(regexp_replace(lower(coalesce(nullif(NEW.repository,''),v.repository)),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','') AS repository_identity,
   CASE WHEN NEW.repository<>'' AND v.repository<>'' THEN regexp_replace(regexp_replace(regexp_replace(lower(NEW.repository),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','') IS DISTINCT FROM regexp_replace(regexp_replace(regexp_replace(lower(v.repository),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','') ELSE false END AS repository_differs
  INTO expected FROM public.native_attempts a
  JOIN public.change_versions v ON v.organization_id=a.organization_id AND v.id=NEW.version_id
  JOIN public.change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id
  JOIN public.work_item_identities i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id
  WHERE a.organization_id=NEW.organization_id AND a.id=NEW.attempt_id;
  IF NOT FOUND OR expected.repository_differs
   OR NEW.issue_id IS NOT NULL AND NEW.issue_id IS DISTINCT FROM expected.issue_id
   OR NEW.fencing_token IS NOT NULL AND NEW.fencing_token IS DISTINCT FROM expected.fencing_token
   OR NEW.change_id IS NOT NULL AND NEW.change_id IS DISTINCT FROM expected.change_id
   OR NEW.project_id IS NOT NULL AND NEW.project_id IS DISTINCT FROM expected.project_id
   OR NEW.repository_identity IS NOT NULL AND NEW.repository_identity IS DISTINCT FROM expected.repository_identity THEN
    RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='landing receipt identity differs from retained authority';
  END IF;
  NEW.issue_id:=expected.issue_id;
  NEW.fencing_token:=expected.fencing_token;
  NEW.change_id:=expected.change_id;
  NEW.project_id:=expected.project_id;
  NEW.repository_identity:=expected.repository_identity;
  RETURN NEW;
 END IF;
 IF TG_OP='UPDATE'  AND current_user='detent_migration'
 AND OLD.issue_id IS NULL AND OLD.change_id IS NULL AND OLD.fencing_token IS NULL AND OLD.repository_identity IS NULL AND OLD.project_id IS NULL
 AND NEW.issue_id IS NOT NULL AND NEW.change_id IS NOT NULL AND NEW.fencing_token IS NOT NULL AND NEW.repository_identity IS NOT NULL AND NEW.project_id IS NOT NULL THEN
  unchanged:=NEW;
  unchanged.issue_id:=OLD.issue_id;
  unchanged.change_id:=OLD.change_id;
  unchanged.fencing_token:=OLD.fencing_token;
  unchanged.repository_identity:=OLD.repository_identity;
  unchanged.project_id:=OLD.project_id;
  IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
 END IF;
 RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='landing receipts are immutable';
END;
$$;
-- +goose StatementEnd
DROP TRIGGER change_landing_receipts_immutable ON change_landing_receipts;
CREATE TRIGGER change_landing_receipts_immutable BEFORE INSERT OR UPDATE OR DELETE ON change_landing_receipts FOR EACH ROW EXECUTE FUNCTION authorize_landing_receipt_initialization();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'landing receipt identity cannot be discarded'; END; $$;
-- +goose StatementEnd
