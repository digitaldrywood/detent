-- +goose Up
ALTER TABLE artifact_catalog_objects
 ADD COLUMN write_state text NOT NULL DEFAULT 'ready' CHECK(write_state IN ('ready','writing','uncertain','written','rejected')),
 ADD COLUMN write_id text NOT NULL DEFAULT '';
ALTER TABLE artifact_catalog_manifest_intents
 ADD COLUMN write_state text NOT NULL DEFAULT 'ready' CHECK(write_state IN ('ready','writing','uncertain','written','rejected')),
 ADD COLUMN write_id text NOT NULL DEFAULT '';
ALTER TABLE artifact_catalog_objects NO FORCE ROW LEVEL SECURITY;
UPDATE artifact_catalog_objects SET write_id='write_'||md5(concat_ws('/',organization_id,project_id,service_id,upload_id,id)),write_state=CASE WHEN verified_at IS NOT NULL THEN 'written' WHEN discarded_at IS NOT NULL THEN 'rejected' ELSE 'uncertain' END;
ALTER TABLE artifact_catalog_objects FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_manifest_intents NO FORCE ROW LEVEL SECURITY;
UPDATE artifact_catalog_manifest_intents SET write_id='write_'||md5(concat_ws('/',organization_id,project_id,service_id,upload_id,id)),write_state=CASE WHEN storage_version<>'' THEN 'written' WHEN state='discarded' THEN 'rejected' ELSE 'uncertain' END;
ALTER TABLE artifact_catalog_manifest_intents FORCE ROW LEVEL SECURITY;

-- +goose Down
ALTER TABLE artifact_catalog_manifest_intents DROP COLUMN write_id,DROP COLUMN write_state;
ALTER TABLE artifact_catalog_objects DROP COLUMN write_id,DROP COLUMN write_state;
