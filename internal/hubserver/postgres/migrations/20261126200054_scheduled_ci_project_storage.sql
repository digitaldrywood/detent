-- +goose Up
CREATE TABLE scheduled_ci_project_storage (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 collaboration_bytes bigint NOT NULL CHECK (collaboration_bytes >= 0),
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
ALTER TABLE scheduled_ci_project_storage OWNER TO detent_migration;
REVOKE ALL ON scheduled_ci_project_storage FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE ON scheduled_ci_project_storage TO detent_runtime;
ALTER TABLE scheduled_ci_project_storage ENABLE ROW LEVEL SECURITY;
ALTER TABLE scheduled_ci_project_storage FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON scheduled_ci_project_storage TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

ALTER TABLE scheduled_ci_decisions ADD COLUMN project_storage_accounted boolean NOT NULL DEFAULT false;
ALTER TABLE scheduled_ci_decisions ALTER COLUMN project_storage_accounted SET DEFAULT true;

-- +goose StatementBegin
CREATE FUNCTION account_scheduled_ci_project_storage() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE delta bigint := NEW.collaboration_bytes;
BEGIN
 IF TG_OP = 'UPDATE' AND OLD.project_storage_accounted THEN
  delta := delta - OLD.collaboration_bytes;
 END IF;
 IF delta <> 0 THEN
  UPDATE public.scheduled_ci_project_storage SET collaboration_bytes=collaboration_bytes+delta
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id;
  IF NOT FOUND THEN
   INSERT INTO public.scheduled_ci_project_storage (organization_id,project_id,collaboration_bytes)
    VALUES (NEW.organization_id,NEW.project_id,delta)
   ON CONFLICT (organization_id,project_id) DO UPDATE
    SET collaboration_bytes=public.scheduled_ci_project_storage.collaboration_bytes+excluded.collaboration_bytes;
  END IF;
 END IF;
 NEW.project_storage_accounted := true;
 RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION account_scheduled_ci_project_storage() FROM PUBLIC;

CREATE TRIGGER scheduled_ci_project_storage_insert AFTER INSERT ON scheduled_ci_decisions
FOR EACH ROW EXECUTE FUNCTION account_scheduled_ci_project_storage();
CREATE TRIGGER scheduled_ci_project_storage_update BEFORE UPDATE OF collaboration_bytes,project_storage_accounted ON scheduled_ci_decisions
FOR EACH ROW EXECUTE FUNCTION account_scheduled_ci_project_storage();

-- +goose Down
DROP TRIGGER scheduled_ci_project_storage_update ON scheduled_ci_decisions;
DROP TRIGGER scheduled_ci_project_storage_insert ON scheduled_ci_decisions;
DROP FUNCTION account_scheduled_ci_project_storage();
ALTER TABLE scheduled_ci_decisions DROP COLUMN project_storage_accounted;
DROP TABLE scheduled_ci_project_storage;
