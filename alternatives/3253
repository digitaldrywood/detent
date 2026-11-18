-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS runner_enrollment_projects_project ON runner_enrollment_projects (organization_id, project_id, enrollment_id);
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace
  AND c.relname='runner_enrollment_projects_project'
  AND (NOT i.indisvalid OR NOT i.indisready)
 ) THEN RAISE EXCEPTION 'Runner enrollment project index requires repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS runner_enrollment_projects_project;
RESET lock_timeout;
