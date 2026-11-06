-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS artifact_upload_authorizations_attempt_key ON artifact_upload_authorizations
 (organization_id, project_id, service_id, attempt_id, idempotency_key);
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relnamespace='public'::regnamespace AND c.relname='artifact_upload_authorizations_attempt_key'
  AND (NOT i.indisvalid OR NOT i.indisready)
 ) THEN RAISE EXCEPTION 'Artifact upload attempt key index requires repair'; END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE artifact_upload_authorizations
 DROP CONSTRAINT artifact_upload_authorizations_pkey,
 ADD CONSTRAINT artifact_upload_authorizations_pkey PRIMARY KEY USING INDEX artifact_upload_authorizations_attempt_key;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS artifact_upload_authorizations_service_key ON artifact_upload_authorizations
 (organization_id, project_id, service_id, idempotency_key);
ALTER TABLE artifact_upload_authorizations
 DROP CONSTRAINT artifact_upload_authorizations_pkey,
 ADD CONSTRAINT artifact_upload_authorizations_pkey PRIMARY KEY USING INDEX artifact_upload_authorizations_service_key;
RESET lock_timeout;
