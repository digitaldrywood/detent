-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE scheduled_ci_decisions ADD COLUMN IF NOT EXISTS collaboration_bytes bigint NOT NULL DEFAULT 0 CHECK (collaboration_bytes >= 0);
ALTER TABLE scheduled_ci_decisions ALTER COLUMN collaboration_bytes DROP DEFAULT;

DROP INDEX CONCURRENTLY IF EXISTS scheduled_ci_decisions_unaccounted_idx;
CREATE INDEX CONCURRENTLY scheduled_ci_decisions_unaccounted_idx ON scheduled_ci_decisions
 (organization_id,project_id,repository,run_id,run_attempt,job_id) WHERE collaboration_bytes=0;

DROP POLICY IF EXISTS scheduled_ci_storage_migration_orgs ON organizations;
CREATE POLICY scheduled_ci_storage_migration_orgs ON organizations FOR SELECT TO detent_migration USING (true);

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE backfill_scheduled_ci_decision_storage()
LANGUAGE plpgsql AS $$
DECLARE org text; previous_org text; batch_bytes bigint;
BEGIN
 LOOP
  SELECT organization_id INTO org FROM public.organizations
   WHERE previous_org IS NULL OR organization_id > previous_org
   ORDER BY organization_id LIMIT 1;
  EXIT WHEN org IS NULL;
  LOOP
   PERFORM set_config('detent.organization_id',org,true);
   WITH pending AS (
    SELECT organization_id,project_id,repository,run_id,run_attempt,job_id
    FROM public.scheduled_ci_decisions
    WHERE organization_id=org AND collaboration_bytes=0
    ORDER BY project_id,repository,run_id,run_attempt,job_id
    LIMIT 128 FOR UPDATE
   ), charged AS (
    UPDATE public.scheduled_ci_decisions d SET collaboration_bytes=
     octet_length(d.organization_id)::bigint + octet_length(d.project_id) + octet_length(d.repository)
     + octet_length(d.head_sha) + octet_length(d.run_id) + octet_length(d.run_attempt) + octet_length(d.job_id)
     + octet_length(d.outcome) + octet_length(d.target) + octet_length(d.reason) + 8
    FROM pending p
    WHERE (d.organization_id,d.project_id,d.repository,d.run_id,d.run_attempt,d.job_id)
        = (p.organization_id,p.project_id,p.repository,p.run_id,p.run_attempt,p.job_id)
    RETURNING d.collaboration_bytes
   )
   SELECT COALESCE(sum(collaboration_bytes),0) INTO batch_bytes FROM charged;
   IF batch_bytes > 0 THEN
    INSERT INTO public.hosted_usage_counters(organization_id,name,value)
     VALUES(org,'collaboration_bytes',batch_bytes)
    ON CONFLICT (organization_id,name) DO UPDATE
     SET value=public.hosted_usage_counters.value+excluded.value;
   END IF;
   COMMIT;
   EXIT WHEN batch_bytes=0;
  END LOOP;
  previous_org:=org;
 END LOOP;
END $$;
-- +goose StatementEnd
REVOKE ALL ON PROCEDURE backfill_scheduled_ci_decision_storage() FROM PUBLIC;
CALL backfill_scheduled_ci_decision_storage();
DROP PROCEDURE backfill_scheduled_ci_decision_storage();
DROP POLICY scheduled_ci_storage_migration_orgs ON organizations;
DROP INDEX CONCURRENTLY scheduled_ci_decisions_unaccounted_idx;
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'scheduled CI decision storage accounting is forward-only'; END $$;
-- +goose StatementEnd
