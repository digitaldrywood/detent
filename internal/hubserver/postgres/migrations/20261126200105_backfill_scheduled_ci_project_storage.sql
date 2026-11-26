-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS scheduled_ci_project_storage_pending_idx;
CREATE INDEX CONCURRENTLY scheduled_ci_project_storage_pending_idx ON scheduled_ci_decisions
 (organization_id,project_id,repository,run_id,run_attempt,job_id) WHERE NOT project_storage_accounted;

DROP POLICY IF EXISTS scheduled_ci_project_storage_orgs ON organizations;
CREATE POLICY scheduled_ci_project_storage_orgs ON organizations FOR SELECT TO detent_migration USING (true);

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE backfill_scheduled_ci_project_storage()
LANGUAGE plpgsql AS $$
DECLARE org text; previous_org text; project text;
BEGIN
 LOOP
  SELECT organization_id INTO org FROM public.organizations
   WHERE previous_org IS NULL OR organization_id > previous_org
   ORDER BY organization_id LIMIT 1;
  EXIT WHEN org IS NULL;
  LOOP
   PERFORM set_config('detent.organization_id',org,true);
   SELECT project_id INTO project FROM public.scheduled_ci_decisions
    WHERE organization_id=org AND NOT project_storage_accounted
    ORDER BY project_id LIMIT 1;
   EXIT WHEN project IS NULL;
   PERFORM id FROM public.projects WHERE organization_id=org AND id=project FOR NO KEY UPDATE;
   WITH pending AS (
    SELECT organization_id,project_id,repository,run_id,run_attempt,job_id
    FROM public.scheduled_ci_decisions
    WHERE organization_id=org AND project_id=project AND NOT project_storage_accounted
    ORDER BY repository,run_id,run_attempt,job_id LIMIT 128
   )
   UPDATE public.scheduled_ci_decisions d SET project_storage_accounted=true
   FROM pending p
   WHERE (d.organization_id,d.project_id,d.repository,d.run_id,d.run_attempt,d.job_id)
       = (p.organization_id,p.project_id,p.repository,p.run_id,p.run_attempt,p.job_id)
     AND NOT d.project_storage_accounted;
   COMMIT;
  END LOOP;
  previous_org:=org;
  COMMIT;
 END LOOP;
END $$;
-- +goose StatementEnd
REVOKE ALL ON PROCEDURE backfill_scheduled_ci_project_storage() FROM PUBLIC;
CALL backfill_scheduled_ci_project_storage();
DROP PROCEDURE backfill_scheduled_ci_project_storage();
DROP POLICY scheduled_ci_project_storage_orgs ON organizations;
DROP INDEX CONCURRENTLY scheduled_ci_project_storage_pending_idx;

-- +goose Down
SELECT 1;
