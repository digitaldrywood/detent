-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE issues ADD COLUMN doctor_identity_revision bigint;
CREATE TABLE doctor_proposal_identities (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('complete','marker','fingerprint')),
 identity_hash text NOT NULL CHECK (identity_hash ~ '^[0-9a-f]{64}$'),
 identity_value bytea NOT NULL,
 source_revision bigint NOT NULL CHECK (source_revision>0),
 version_sha256 text NOT NULL CHECK (version_sha256 ~ '^[0-9a-f]{64}$'),
 has_state boolean NOT NULL,
 state_name text NOT NULL,
 archived boolean NOT NULL,
 item_number bigint NOT NULL CHECK (item_number>0),
 PRIMARY KEY (organization_id,project_id,work_item_id,kind,identity_hash),
 FOREIGN KEY (organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id) ON DELETE CASCADE,
 CHECK (has_state OR state_name=''),
 CHECK (kind<>'complete' OR octet_length(identity_value)=0)
);
ALTER TABLE doctor_proposal_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE doctor_proposal_identities FORCE ROW LEVEL SECURITY;
REVOKE ALL ON doctor_proposal_identities FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON doctor_proposal_identities TO detent_runtime;
CREATE POLICY tenant_isolation ON doctor_proposal_identities TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose StatementBegin
CREATE FUNCTION invalidate_doctor_proposal_identities() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='INSERT' OR NEW.revision IS DISTINCT FROM OLD.revision OR NEW.body_object_key IS DISTINCT FROM OLD.body_object_key OR NEW.native_id IS DISTINCT FROM OLD.native_id THEN NEW.doctor_identity_revision:=NULL; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION invalidate_doctor_proposal_identities() FROM PUBLIC;
CREATE TRIGGER doctor_proposal_source_changed BEFORE INSERT OR UPDATE OF revision,body_object_key,native_id ON issues FOR EACH ROW EXECUTE FUNCTION invalidate_doctor_proposal_identities();
-- +goose StatementBegin
CREATE FUNCTION refresh_doctor_proposal_scope() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 UPDATE public.doctor_proposal_identities SET has_state=NEW.workflow_state_name IS NOT NULL,state_name=coalesce(NEW.workflow_state_name,''),archived=NEW.archived,item_number=NEW.number WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND work_item_id=NEW.native_id;
 IF NEW.number IS DISTINCT FROM OLD.number THEN UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION refresh_doctor_proposal_scope() FROM PUBLIC;
CREATE TRIGGER doctor_proposal_scope_changed AFTER UPDATE OF workflow_state_name,archived,number ON issues FOR EACH ROW WHEN ((NEW.workflow_state_name,NEW.archived,NEW.number) IS DISTINCT FROM (OLD.workflow_state_name,OLD.archived,OLD.number)) EXECUTE FUNCTION refresh_doctor_proposal_scope();
-- +goose StatementBegin
CREATE FUNCTION advance_doctor_proposal_source() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=OLD.organization_id AND id=OLD.project_id;
  RETURN OLD;
 END IF;
 UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION advance_doctor_proposal_source() FROM PUBLIC;
CREATE TRIGGER doctor_proposal_issue_source AFTER INSERT OR DELETE ON issues FOR EACH ROW EXECUTE FUNCTION advance_doctor_proposal_source();
CREATE TRIGGER doctor_proposal_workflow_source AFTER INSERT OR DELETE OR UPDATE OF terminal,name ON project_workflow_states FOR EACH ROW EXECUTE FUNCTION advance_doctor_proposal_source();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER doctor_proposal_workflow_source ON project_workflow_states;
DROP TRIGGER doctor_proposal_issue_source ON issues;
DROP FUNCTION advance_doctor_proposal_source();
DROP TRIGGER doctor_proposal_scope_changed ON issues;
DROP TRIGGER doctor_proposal_source_changed ON issues;
DROP FUNCTION refresh_doctor_proposal_scope();
DROP FUNCTION invalidate_doctor_proposal_identities();
DROP TABLE doctor_proposal_identities;
ALTER TABLE issues DROP COLUMN doctor_identity_revision;
