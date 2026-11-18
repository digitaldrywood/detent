-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE initialize_local_board_populations()
LANGUAGE plpgsql AS $$
DECLARE
 org text;
 generation bigint;
 current_generation bigint;
 complete boolean;
 lower_id bigint;
 upper_id bigint;
 active_baseline bigint;
 history_baseline bigint;
BEGIN
 CREATE TEMP TABLE IF NOT EXISTS local_board_population_snapshot (
  row_id bigint GENERATED ALWAYS AS IDENTITY,
  project_id text NOT NULL,
  kind text NOT NULL,
  has_state boolean NOT NULL,
  state_name text NOT NULL,
  baseline bigint NOT NULL
 ) ON COMMIT PRESERVE ROWS;
 COMMIT;
 FOR org IN SELECT organization_id FROM public.organizations ORDER BY organization_id LOOP
  PERFORM set_config('detent.organization_id',org,true);
  SELECT bool_and(initialized) AND count(*)=2 INTO complete FROM public.hosted_usage_counters WHERE organization_id=org AND name IN ('unarchived_issues','history_records');
  COMMIT;
  IF complete THEN CONTINUE; END IF;
  LOOP
   PERFORM set_config('detent.organization_id',org,true);
   TRUNCATE pg_temp.local_board_population_snapshot RESTART IDENTITY;
   WITH state_counts AS MATERIALIZED (
    SELECT i.project_id,i.workflow_state_name IS NOT NULL AS has_state,coalesce(i.workflow_state_name,'') AS state_name,count(*) AS value
    FROM public.issues i WHERE i.organization_id=org AND NOT i.archived
    GROUP BY i.project_id,i.workflow_state_name
   ), project_counts AS MATERIALIZED (
    SELECT s.project_id,sum(s.value)::bigint AS value FROM state_counts s
    LEFT JOIN public.project_workflow_states w ON w.organization_id=org AND w.project_id=s.project_id AND s.has_state AND w.name=s.state_name
    WHERE NOT coalesce(w.terminal,false) GROUP BY s.project_id
   ), actual AS (
    SELECT project_id,'state'::text AS kind,has_state,state_name,value FROM state_counts
    UNION ALL SELECT project_id,'project',false,'',value FROM project_counts
   ), delta AS (
    SELECT project_id,kind,has_state,state_name,value FROM public.local_board_populations WHERE organization_id=org
   ), baselines AS (
    SELECT coalesce(a.project_id,d.project_id) AS project_id,coalesce(a.kind,d.kind) AS kind,coalesce(a.has_state,d.has_state) AS has_state,coalesce(a.state_name,d.state_name) AS state_name,coalesce(a.value,0)-coalesce(d.value,0) AS value
    FROM actual a FULL JOIN delta d USING(project_id,kind,has_state,state_name)
   ) INSERT INTO pg_temp.local_board_population_snapshot(project_id,kind,has_state,state_name,baseline)
    SELECT project_id,kind,has_state,state_name,value FROM baselines
    UNION ALL SELECT '','global',false,'unarchived_issues',
     coalesce((SELECT sum(c.value)::bigint FROM project_counts c JOIN public.projects p ON p.organization_id=org AND p.id=c.project_id WHERE p.profile='native' AND p.deleted_at IS NULL),0)-(SELECT value FROM public.hosted_usage_counters WHERE organization_id=org AND name='unarchived_issues')
    UNION ALL SELECT '','global',false,'history_records',
     (SELECT count(*) FROM public.collaboration_versions WHERE organization_id=org)+(SELECT count(*) FROM public.collaboration_events WHERE organization_id=org)+(SELECT count(*) FROM public.native_attempt_events WHERE organization_id=org)-(SELECT value FROM public.hosted_usage_counters WHERE organization_id=org AND name='history_records')
    UNION ALL SELECT '','generation',false,'',population_revision FROM public.archive_usage_counters WHERE organization_id=org;
   COMMIT;
   SELECT baseline INTO STRICT generation FROM pg_temp.local_board_population_snapshot WHERE kind='generation';
   SELECT baseline INTO STRICT active_baseline FROM pg_temp.local_board_population_snapshot WHERE kind='global' AND state_name='unarchived_issues';
   SELECT baseline INTO STRICT history_baseline FROM pg_temp.local_board_population_snapshot WHERE kind='global' AND state_name='history_records';
   lower_id:=0;
   LOOP
    SELECT max(row_id) INTO upper_id FROM (SELECT row_id FROM pg_temp.local_board_population_snapshot WHERE row_id>lower_id AND kind IN ('state','project') ORDER BY row_id LIMIT 200) page;
    EXIT WHEN upper_id IS NULL;
    PERFORM set_config('detent.organization_id',org,true);
    PERFORM p.id FROM public.projects p WHERE p.organization_id=org AND p.id IN (SELECT project_id FROM pg_temp.local_board_population_snapshot WHERE row_id>lower_id AND row_id<=upper_id) ORDER BY p.id FOR KEY SHARE;
    SELECT population_revision INTO STRICT current_generation FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
    SELECT bool_and(initialized) AND count(*)=2 INTO complete FROM public.hosted_usage_counters WHERE organization_id=org AND name IN ('unarchived_issues','history_records');
    IF current_generation<>generation OR complete THEN COMMIT; EXIT; END IF;
    INSERT INTO public.local_board_populations(organization_id,project_id,kind,has_state,state_name,value)
     SELECT org,s.project_id,s.kind,s.has_state,s.state_name,s.baseline FROM pg_temp.local_board_population_snapshot s
     JOIN public.projects p ON p.organization_id=org AND p.id=s.project_id WHERE s.row_id>lower_id AND s.row_id<=upper_id
     ON CONFLICT(organization_id,project_id,kind,has_state,state_name) DO UPDATE SET value=public.local_board_populations.value+excluded.value;
    DELETE FROM public.local_board_populations p USING pg_temp.local_board_population_snapshot s
     WHERE p.organization_id=org AND (p.project_id,p.kind,p.has_state,p.state_name)=(s.project_id,s.kind,s.has_state,s.state_name) AND s.row_id>lower_id AND s.row_id<=upper_id AND p.value=0;
    COMMIT;
    lower_id:=upper_id;
   END LOOP;
   EXIT WHEN complete;
   IF current_generation IS DISTINCT FROM generation AND upper_id IS NOT NULL THEN CONTINUE; END IF;
   PERFORM set_config('detent.organization_id',org,true);
   SELECT population_revision INTO STRICT current_generation FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
   SELECT bool_and(initialized) AND count(*)=2 INTO complete FROM public.hosted_usage_counters WHERE organization_id=org AND name IN ('unarchived_issues','history_records');
   IF current_generation=generation AND NOT complete THEN
    UPDATE public.hosted_usage_counters SET value=value+CASE name WHEN 'unarchived_issues' THEN active_baseline ELSE history_baseline END,initialized=true WHERE organization_id=org AND name IN ('unarchived_issues','history_records');
    IF EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name IN ('unarchived_issues','history_records') AND value<0) THEN RAISE EXCEPTION 'invalid population initialization'; END IF;
    complete:=true;
   END IF;
   COMMIT;
   EXIT WHEN complete;
  END LOOP;
 END LOOP;
 DROP TABLE pg_temp.local_board_population_snapshot;
END $$;
-- +goose StatementEnd
REVOKE ALL ON PROCEDURE initialize_local_board_populations() FROM PUBLIC;
CALL initialize_local_board_populations();
DROP PROCEDURE initialize_local_board_populations();
DROP POLICY IF EXISTS local_board_initialization_orgs ON organizations;

-- +goose Down
SELECT 1;
