-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS artifact_services_hosted_publisher_idx ON artifact_services
 (organization_id, id, publisher_token_id) INCLUDE (project_id) WHERE binding_mode='hosted' AND hosted_opt_in;
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace AND c.relname='artifact_services_hosted_publisher_idx'
  AND (NOT i.indisvalid OR NOT i.indisready)
 ) THEN RAISE EXCEPTION 'Hosted artifact publisher index requires repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS artifact_services_hosted_publisher_idx;
RESET lock_timeout;
