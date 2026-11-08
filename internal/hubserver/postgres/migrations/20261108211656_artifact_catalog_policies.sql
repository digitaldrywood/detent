-- +goose Up
ALTER TABLE artifact_services
 ADD COLUMN catalog_policy_id text NOT NULL DEFAULT '' CHECK(catalog_policy_id='' OR length(btrim(catalog_policy_id)) BETWEEN 1 AND 128),
 ADD COLUMN catalog_retained_limit bigint NOT NULL DEFAULT 1152921504606846976 CHECK(catalog_retained_limit>0 AND catalog_retained_limit<=1152921504606846976),
 ADD COLUMN catalog_reserved_limit bigint NOT NULL DEFAULT 1152921504606846976 CHECK(catalog_reserved_limit>0 AND catalog_reserved_limit<=1152921504606846976),
 ADD COLUMN catalog_artifact_limit bigint NOT NULL DEFAULT 268435456 CHECK(catalog_artifact_limit>0 AND catalog_artifact_limit<=268435456),
 ADD COLUMN catalog_upload_limit bigint NOT NULL DEFAULT 67108864 CHECK(catalog_upload_limit>0 AND catalog_upload_limit<=67108864),
 ADD COLUMN catalog_retention_seconds bigint NOT NULL DEFAULT 315360000 CHECK(catalog_retention_seconds>0 AND catalog_retention_seconds<=315360000),
 ADD COLUMN catalog_abandoned_seconds bigint NOT NULL DEFAULT 31536000 CHECK(catalog_abandoned_seconds>0 AND catalog_abandoned_seconds<=31536000),
 ADD COLUMN catalog_deletion_seconds bigint NOT NULL DEFAULT 34560000 CHECK(catalog_deletion_seconds>0 AND catalog_deletion_seconds<=315360000),
 ADD COLUMN catalog_backup_seconds bigint NOT NULL DEFAULT 86400 CHECK(catalog_backup_seconds>0 AND catalog_backup_seconds<=31536000);
ALTER TABLE artifact_services ADD CONSTRAINT artifact_catalog_policy_lifetime CHECK(catalog_deletion_seconds>catalog_abandoned_seconds+catalog_backup_seconds);
ALTER TABLE artifact_catalog_uploads
 ADD COLUMN binding_mode text NOT NULL DEFAULT 'customer' CHECK(binding_mode IN ('customer','hosted')),
 ADD COLUMN hosted_opt_in boolean NOT NULL DEFAULT false;
ALTER TABLE artifact_catalog_uploads NO FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_services NO FORCE ROW LEVEL SECURITY;
UPDATE artifact_catalog_uploads u SET binding_mode=b.binding_mode,hosted_opt_in=b.hosted_opt_in FROM artifact_services b WHERE (b.organization_id,b.project_id,b.id)=(u.organization_id,u.project_id,u.service_id);
ALTER TABLE artifact_catalog_uploads FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_services FORCE ROW LEVEL SECURITY;

-- +goose Down
ALTER TABLE artifact_catalog_uploads DROP COLUMN hosted_opt_in,DROP COLUMN binding_mode;
ALTER TABLE artifact_services DROP CONSTRAINT artifact_catalog_policy_lifetime;
ALTER TABLE artifact_services DROP COLUMN catalog_backup_seconds,DROP COLUMN catalog_deletion_seconds,DROP COLUMN catalog_abandoned_seconds,DROP COLUMN catalog_retention_seconds,DROP COLUMN catalog_upload_limit,DROP COLUMN catalog_artifact_limit,DROP COLUMN catalog_reserved_limit,DROP COLUMN catalog_retained_limit,DROP COLUMN catalog_policy_id;
