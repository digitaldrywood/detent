-- +goose Up
SET LOCAL lock_timeout = '5s';
DROP TRIGGER local_board_issue_population ON issues;
DROP TRIGGER local_board_issue_population_update ON issues;
DROP TRIGGER doctor_proposal_issue_source ON issues;

-- +goose StatementBegin
CREATE FUNCTION local_board_issue_statement_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
 previous_scope text:=current_setting('detent.organization_id',true);
 sources text;
 scope record;
 delta record;
 organizations text[]:='{}';
 org text;
 previous_org text;
 ready boolean;
 population bigint;
 active_org text;
 active_project text;
 project_present boolean;
 project_eligible boolean;
 terminal boolean;
 project_change bigint:=0;
 tenant_change bigint:=0;
BEGIN
 IF TG_RELID<>'public.issues'::regclass OR TG_OP NOT IN ('INSERT','UPDATE','DELETE') THEN RAISE EXCEPTION 'invalid issue population source'; END IF;
 IF TG_OP='INSERT' THEN
  sources:='SELECT organization_id,project_id,archived,workflow_state_name,1::bigint AS amount FROM new_issue_rows';
 ELSIF TG_OP='DELETE' THEN
  sources:='SELECT organization_id,project_id,archived,workflow_state_name,-1::bigint AS amount FROM old_issue_rows';
 ELSE
  sources:='SELECT n.organization_id,n.project_id,n.archived,n.workflow_state_name,1::bigint AS amount FROM new_issue_rows n JOIN old_issue_rows o USING(organization_id,id) WHERE (n.archived,n.workflow_state_name) IS DISTINCT FROM (o.archived,o.workflow_state_name)
   UNION ALL SELECT o.organization_id,o.project_id,o.archived,o.workflow_state_name,-1::bigint AS amount FROM old_issue_rows o JOIN new_issue_rows n USING(organization_id,id) WHERE (n.archived,n.workflow_state_name) IS DISTINCT FROM (o.archived,o.workflow_state_name)';
 END IF;
 FOR scope IN EXECUTE 'SELECT organization_id,project_id FROM ('||sources||') s GROUP BY organization_id,project_id ORDER BY organization_id COLLATE "C",project_id COLLATE "C"' LOOP
  PERFORM set_config('detent.organization_id',scope.organization_id,true);
  PERFORM 1 FROM public.projects WHERE organization_id=scope.organization_id AND id=scope.project_id FOR KEY SHARE;
  IF FOUND AND previous_org IS DISTINCT FROM scope.organization_id THEN
   organizations:=array_append(organizations,scope.organization_id);
   previous_org:=scope.organization_id;
  END IF;
 END LOOP;
 FOREACH org IN ARRAY organizations LOOP
  PERFORM set_config('detent.organization_id',org,true);
  PERFORM 1 FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
  SELECT initialized INTO ready FROM public.hosted_usage_counters WHERE organization_id=org AND name='unarchived_issues';
  IF ready IS NULL THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 END LOOP;
 FOR delta IN EXECUTE 'SELECT organization_id,project_id,workflow_state_name IS NOT NULL AS has_state,coalesce(workflow_state_name,'''') AS state_name,sum(amount)::bigint AS amount
  FROM ('||sources||') s WHERE NOT archived GROUP BY organization_id,project_id,workflow_state_name HAVING sum(amount)<>0
  ORDER BY organization_id COLLATE "C",project_id COLLATE "C",has_state,state_name' LOOP
  IF active_org IS DISTINCT FROM delta.organization_id OR active_project IS DISTINCT FROM delta.project_id THEN
   IF project_present THEN
    PERFORM public.local_board_population_delta(active_org,active_project,'project',false,'',project_change);
   END IF;
   IF active_org IS DISTINCT FROM delta.organization_id THEN
    IF tenant_change<>0 THEN
     UPDATE public.hosted_usage_counters SET value=value+tenant_change WHERE organization_id=active_org AND name='unarchived_issues';
     IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
    END IF;
    tenant_change:=0;
    active_org:=delta.organization_id;
    PERFORM set_config('detent.organization_id',active_org,true);
   END IF;
   active_project:=delta.project_id;
   project_change:=0;
   SELECT p.profile='native' AND p.deleted_at IS NULL INTO project_eligible FROM public.projects p WHERE p.organization_id=active_org AND p.id=active_project;
   project_present:=FOUND;
  END IF;
  IF NOT project_present THEN CONTINUE; END IF;
  PERFORM public.local_board_population_delta(active_org,active_project,'state',delta.has_state,delta.state_name,delta.amount);
  SELECT coalesce((SELECT w.terminal FROM public.project_workflow_states w WHERE w.organization_id=active_org AND w.project_id=active_project AND delta.has_state AND w.name=delta.state_name),false) INTO terminal;
  IF NOT terminal THEN
   project_change:=project_change+delta.amount;
   IF project_eligible THEN tenant_change:=tenant_change+delta.amount; END IF;
  END IF;
 END LOOP;
 IF project_present THEN
  PERFORM public.local_board_population_delta(active_org,active_project,'project',false,'',project_change);
 END IF;
 IF tenant_change<>0 THEN
  UPDATE public.hosted_usage_counters SET value=value+tenant_change WHERE organization_id=active_org AND name='unarchived_issues';
  IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 END IF;
 FOREACH org IN ARRAY organizations LOOP
  PERFORM set_config('detent.organization_id',org,true);
  SELECT initialized,value INTO ready,population FROM public.hosted_usage_counters WHERE organization_id=org AND name='unarchived_issues';
  IF ready IS NULL THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
  IF ready AND population<0 THEN RAISE EXCEPTION 'negative active population'; END IF;
 END LOOP;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RETURN NULL;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION local_board_issue_statement_population() FROM PUBLIC;
CREATE TRIGGER local_board_issue_population AFTER INSERT ON issues
REFERENCING NEW TABLE AS new_issue_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_issue_statement_population();
CREATE TRIGGER local_board_issue_population_delete AFTER DELETE ON issues
REFERENCING OLD TABLE AS old_issue_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_issue_statement_population();
CREATE TRIGGER local_board_issue_population_update AFTER UPDATE ON issues
REFERENCING OLD TABLE AS old_issue_rows NEW TABLE AS new_issue_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_issue_statement_population();

-- +goose StatementBegin
CREATE FUNCTION advance_doctor_proposal_issue_source() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE source record;
BEGIN
 IF TG_RELID<>'public.issues'::regclass OR TG_OP NOT IN ('INSERT','DELETE') THEN RAISE EXCEPTION 'invalid doctor issue source'; END IF;
 FOR source IN SELECT organization_id,project_id,count(*) AS amount FROM doctor_issue_rows GROUP BY organization_id,project_id ORDER BY organization_id COLLATE "C",project_id COLLATE "C" LOOP
  UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+source.amount WHERE organization_id=source.organization_id AND id=source.project_id;
 END LOOP;
 RETURN NULL;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION advance_doctor_proposal_issue_source() FROM PUBLIC;
CREATE TRIGGER doctor_proposal_issue_source AFTER INSERT ON issues
REFERENCING NEW TABLE AS doctor_issue_rows FOR EACH STATEMENT EXECUTE FUNCTION advance_doctor_proposal_issue_source();
CREATE TRIGGER doctor_proposal_issue_source_delete AFTER DELETE ON issues
REFERENCING OLD TABLE AS doctor_issue_rows FOR EACH STATEMENT EXECUTE FUNCTION advance_doctor_proposal_issue_source();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER doctor_proposal_issue_source_delete ON issues;
DROP TRIGGER doctor_proposal_issue_source ON issues;
DROP FUNCTION advance_doctor_proposal_issue_source();
CREATE TRIGGER doctor_proposal_issue_source AFTER INSERT OR DELETE ON issues FOR EACH ROW EXECUTE FUNCTION advance_doctor_proposal_source();
DROP TRIGGER local_board_issue_population_update ON issues;
DROP TRIGGER local_board_issue_population_delete ON issues;
DROP TRIGGER local_board_issue_population ON issues;
DROP FUNCTION local_board_issue_statement_population();
CREATE TRIGGER local_board_issue_population AFTER INSERT OR DELETE ON issues
FOR EACH ROW EXECUTE FUNCTION local_board_source_population();
CREATE TRIGGER local_board_issue_population_update AFTER UPDATE OF archived,workflow_state_name ON issues
FOR EACH ROW WHEN ((OLD.organization_id,OLD.project_id,OLD.archived,OLD.workflow_state_name)
 IS DISTINCT FROM (NEW.organization_id,NEW.project_id,NEW.archived,NEW.workflow_state_name))
EXECUTE FUNCTION local_board_source_population();
