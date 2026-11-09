-- +goose Up
SET LOCAL lock_timeout = '5s';
LOCK TABLE organizations IN SHARE ROW EXCLUSIVE MODE;
ALTER TABLE hosted_usage_counters ADD COLUMN initialized boolean NOT NULL DEFAULT true;
ALTER TABLE archive_usage_counters ADD COLUMN population_revision bigint NOT NULL DEFAULT 0;
CREATE TABLE local_board_populations (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('state','project')),
 has_state boolean NOT NULL,
 state_name text NOT NULL,
 value bigint NOT NULL,
 PRIMARY KEY (organization_id,project_id,kind,has_state,state_name),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE,
 CHECK (has_state OR state_name=''),
 CHECK (kind='state' OR (NOT has_state AND state_name=''))
);
ALTER TABLE local_board_populations ENABLE ROW LEVEL SECURITY;
ALTER TABLE local_board_populations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON local_board_populations FROM PUBLIC, detent_runtime, detent_platform;
GRANT SELECT ON local_board_populations TO detent_runtime;
CREATE POLICY tenant_read ON local_board_populations FOR SELECT TO detent_runtime USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_owner ON local_board_populations TO detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
DROP POLICY tenant_isolation ON hosted_usage_counters;
CREATE POLICY tenant_owner ON hosted_usage_counters TO detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_read ON hosted_usage_counters FOR SELECT TO detent_runtime USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_insert ON hosted_usage_counters FOR INSERT TO detent_runtime WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records'));
CREATE POLICY tenant_update ON hosted_usage_counters FOR UPDATE TO detent_runtime USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records'));
CREATE POLICY tenant_delete ON hosted_usage_counters FOR DELETE TO detent_runtime USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records'));
REVOKE INSERT, UPDATE ON hosted_usage_counters FROM detent_runtime;
GRANT INSERT (organization_id,name,value), UPDATE (value) ON hosted_usage_counters TO detent_runtime;
REVOKE INSERT, UPDATE, DELETE ON archive_usage_counters FROM detent_runtime, detent_platform;
GRANT INSERT (organization_id,compressed_bytes), UPDATE (compressed_bytes) ON archive_usage_counters TO detent_runtime, detent_platform;
CREATE POLICY local_board_initialization_orgs ON organizations FOR SELECT TO detent_migration USING (true);
CREATE POLICY local_board_initialization_usage ON hosted_usage_counters FOR INSERT TO detent_migration WITH CHECK (true);
CREATE POLICY local_board_initialization_usage_update ON hosted_usage_counters FOR UPDATE TO detent_migration USING (true) WITH CHECK (true);
CREATE POLICY local_board_initialization_archive ON archive_usage_counters FOR INSERT TO detent_migration WITH CHECK (true);
INSERT INTO hosted_usage_counters(organization_id,name,value,initialized)
 SELECT o.organization_id,m.name,0,false FROM organizations o CROSS JOIN (VALUES ('unarchived_issues'),('history_records')) m(name) ON CONFLICT DO NOTHING;
UPDATE hosted_usage_counters SET initialized=false WHERE name IN ('unarchived_issues','history_records');
INSERT INTO archive_usage_counters(organization_id,compressed_bytes) SELECT organization_id,0 FROM organizations ON CONFLICT DO NOTHING;
DROP POLICY local_board_initialization_usage_update ON hosted_usage_counters;
DROP POLICY local_board_initialization_usage ON hosted_usage_counters;
DROP POLICY local_board_initialization_archive ON archive_usage_counters;

-- +goose StatementBegin
CREATE FUNCTION local_board_population_delta(org text, project text, bucket text, present boolean, state text, amount bigint) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF amount=0 THEN RETURN; END IF;
 INSERT INTO public.local_board_populations(organization_id,project_id,kind,has_state,state_name,value) VALUES(org,project,bucket,present,state,amount)
 ON CONFLICT(organization_id,project_id,kind,has_state,state_name) DO UPDATE SET value=public.local_board_populations.value+excluded.value;
 DELETE FROM public.local_board_populations WHERE organization_id=org AND project_id=project AND kind=bucket AND has_state=present AND state_name=state AND value=0;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION local_board_population_delta(text,text,text,boolean,text,bigint) FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION local_board_source_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
 org text;
 project text;
 previous_scope text:=current_setting('detent.organization_id',true);
 eligible boolean;
 ready boolean;
 old_active boolean:=false;
 new_active boolean:=false;
 state_count bigint;
 change bigint;
BEGIN
 IF TG_RELID NOT IN ('public.issues'::regclass,'public.projects'::regclass,'public.project_workflow_states'::regclass) THEN RAISE EXCEPTION 'invalid population source'; END IF;
 IF TG_OP='DELETE' THEN org:=OLD.organization_id; ELSE org:=NEW.organization_id; END IF;
 IF TG_OP='UPDATE' AND NEW.organization_id IS DISTINCT FROM OLD.organization_id THEN RAISE EXCEPTION 'population scope is immutable'; END IF;
 IF TG_RELID='public.projects'::regclass THEN
  IF TG_OP='DELETE' THEN project:=OLD.id; ELSE project:=NEW.id; END IF;
  IF TG_OP='UPDATE' AND NEW.id IS DISTINCT FROM OLD.id THEN RAISE EXCEPTION 'population scope is immutable'; END IF;
 ELSE
  IF TG_OP='DELETE' THEN project:=OLD.project_id; ELSE project:=NEW.project_id; END IF;
  IF TG_OP='UPDATE' AND NEW.project_id IS DISTINCT FROM OLD.project_id THEN RAISE EXCEPTION 'population scope is immutable'; END IF;
 END IF;
 PERFORM set_config('detent.organization_id',org,true);
 SELECT p.profile='native' AND p.deleted_at IS NULL INTO eligible FROM public.projects p WHERE p.organization_id=org AND p.id=project FOR KEY SHARE;
 IF NOT FOUND THEN
  PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
  RETURN OLD;
 END IF;
 PERFORM 1 FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 SELECT initialized INTO ready FROM public.hosted_usage_counters WHERE organization_id=org AND name='unarchived_issues';
 IF ready IS NULL THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 IF TG_RELID='public.issues'::regclass THEN
  IF TG_OP<>'INSERT' AND NOT OLD.archived THEN
   PERFORM public.local_board_population_delta(org,project,'state',OLD.workflow_state_name IS NOT NULL,coalesce(OLD.workflow_state_name,''),-1);
   SELECT NOT coalesce((SELECT terminal FROM public.project_workflow_states WHERE organization_id=org AND project_id=project AND name=OLD.workflow_state_name),false) INTO old_active;
  END IF;
  IF TG_OP<>'DELETE' AND NOT NEW.archived THEN
   PERFORM public.local_board_population_delta(org,project,'state',NEW.workflow_state_name IS NOT NULL,coalesce(NEW.workflow_state_name,''),1);
   SELECT NOT coalesce((SELECT terminal FROM public.project_workflow_states WHERE organization_id=org AND project_id=project AND name=NEW.workflow_state_name),false) INTO new_active;
  END IF;
  change:=new_active::integer-old_active::integer;
  PERFORM public.local_board_population_delta(org,project,'project',false,'',change);
  IF NOT eligible THEN change:=0; END IF;
 ELSE
  IF NOT ready THEN
   UPDATE public.archive_usage_counters SET population_revision=population_revision+1 WHERE organization_id=org;
   change:=0;
  ELSIF TG_RELID='public.project_workflow_states'::regclass THEN
   IF TG_OP<>'INSERT' THEN old_active:=NOT OLD.terminal; END IF;
   IF TG_OP<>'DELETE' THEN new_active:=NOT NEW.terminal; END IF;
   IF TG_OP='INSERT' THEN old_active:=true; END IF;
   IF TG_OP='DELETE' THEN new_active:=true; END IF;
   IF TG_OP='UPDATE' AND NEW.name IS DISTINCT FROM OLD.name THEN RAISE EXCEPTION 'population state identity is immutable'; END IF;
   SELECT coalesce((SELECT value FROM public.local_board_populations WHERE organization_id=org AND project_id=project AND kind='state' AND has_state AND state_name=coalesce(NEW.name,OLD.name)),0) INTO state_count;
   change:=(new_active::integer-old_active::integer)*state_count;
   PERFORM public.local_board_population_delta(org,project,'project',false,'',change);
   IF NOT eligible THEN change:=0; END IF;
  ELSE
   IF TG_OP<>'INSERT' THEN old_active:=OLD.profile='native' AND OLD.deleted_at IS NULL; END IF;
   IF TG_OP<>'DELETE' THEN new_active:=NEW.profile='native' AND NEW.deleted_at IS NULL; END IF;
   SELECT coalesce((SELECT value FROM public.local_board_populations WHERE organization_id=org AND project_id=project AND kind='project' AND NOT has_state AND state_name=''),0) INTO state_count;
   change:=(new_active::integer-old_active::integer)*state_count;
  END IF;
 END IF;
 UPDATE public.hosted_usage_counters SET value=value+change WHERE organization_id=org AND name='unarchived_issues';
 IF ready AND EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name='unarchived_issues' AND value<0) THEN RAISE EXCEPTION 'negative active population'; END IF;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION local_board_source_population() FROM PUBLIC;
CREATE TRIGGER local_board_issue_population AFTER INSERT OR DELETE OR UPDATE OF archived,workflow_state_name ON issues FOR EACH ROW EXECUTE FUNCTION local_board_source_population();
CREATE TRIGGER local_board_state_population AFTER INSERT OR DELETE OR UPDATE OF terminal,name ON project_workflow_states FOR EACH ROW EXECUTE FUNCTION local_board_source_population();
CREATE TRIGGER local_board_project_population AFTER INSERT OR UPDATE OF profile,deleted_at ON projects FOR EACH ROW EXECUTE FUNCTION local_board_source_population();
CREATE TRIGGER local_board_project_population_delete BEFORE DELETE ON projects FOR EACH ROW EXECUTE FUNCTION local_board_source_population();

-- +goose StatementBegin
CREATE FUNCTION local_board_history_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org text; previous_scope text:=current_setting('detent.organization_id',true); change bigint;
BEGIN
 IF TG_RELID NOT IN ('public.collaboration_versions'::regclass,'public.collaboration_events'::regclass,'public.native_attempt_events'::regclass) OR TG_OP NOT IN ('INSERT','DELETE') THEN RAISE EXCEPTION 'invalid history population source'; END IF;
 IF TG_OP='INSERT' THEN org:=NEW.organization_id; change:=1; ELSE org:=OLD.organization_id; change:=-1; END IF;
 PERFORM set_config('detent.organization_id',org,true);
 PERFORM 1 FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 UPDATE public.hosted_usage_counters SET value=value+change WHERE organization_id=org AND name='history_records';
 IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 IF EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name='history_records' AND initialized AND value<0) THEN RAISE EXCEPTION 'negative history population'; END IF;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION local_board_history_population() FROM PUBLIC;
CREATE TRIGGER local_board_version_population AFTER INSERT OR DELETE ON collaboration_versions FOR EACH ROW EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_event_population AFTER INSERT OR DELETE ON collaboration_events FOR EACH ROW EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_attempt_population AFTER INSERT OR DELETE ON native_attempt_events FOR EACH ROW EXECUTE FUNCTION local_board_history_population();

-- +goose StatementBegin
CREATE FUNCTION local_board_organization_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE previous_scope text:=current_setting('detent.organization_id',true);
BEGIN
 IF TG_RELID<>'public.organizations'::regclass OR TG_OP<>'INSERT' THEN RAISE EXCEPTION 'invalid population organization source'; END IF;
 PERFORM set_config('detent.organization_id',NEW.organization_id,true);
 INSERT INTO public.archive_usage_counters(organization_id,compressed_bytes) VALUES(NEW.organization_id,0) ON CONFLICT DO NOTHING;
 INSERT INTO public.hosted_usage_counters(organization_id,name,value,initialized) VALUES(NEW.organization_id,'unarchived_issues',0,true),(NEW.organization_id,'history_records',0,true) ON CONFLICT DO NOTHING;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RETURN NEW;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION local_board_organization_population() FROM PUBLIC;
CREATE TRIGGER local_board_organization_population AFTER INSERT ON organizations FOR EACH ROW EXECUTE FUNCTION local_board_organization_population();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER local_board_organization_population ON organizations;
DROP TRIGGER local_board_attempt_population ON native_attempt_events;
DROP TRIGGER local_board_event_population ON collaboration_events;
DROP TRIGGER local_board_version_population ON collaboration_versions;
DROP TRIGGER local_board_project_population_delete ON projects;
DROP TRIGGER local_board_project_population ON projects;
DROP TRIGGER local_board_state_population ON project_workflow_states;
DROP TRIGGER local_board_issue_population ON issues;
DROP FUNCTION local_board_organization_population();
DROP FUNCTION local_board_history_population();
DROP FUNCTION local_board_source_population();
DROP FUNCTION local_board_population_delta(text,text,text,boolean,text,bigint);
DROP POLICY IF EXISTS local_board_initialization_orgs ON organizations;
DROP TABLE local_board_populations;
DROP POLICY tenant_owner ON hosted_usage_counters;
DROP POLICY tenant_read ON hosted_usage_counters;
DROP POLICY tenant_insert ON hosted_usage_counters;
DROP POLICY tenant_update ON hosted_usage_counters;
DROP POLICY tenant_delete ON hosted_usage_counters;
CREATE POLICY tenant_isolation ON hosted_usage_counters TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE hosted_usage_counters DROP COLUMN initialized;
ALTER TABLE archive_usage_counters DROP COLUMN population_revision;
GRANT INSERT,UPDATE ON hosted_usage_counters TO detent_runtime;
GRANT INSERT,UPDATE,DELETE ON archive_usage_counters TO detent_runtime,detent_platform;
