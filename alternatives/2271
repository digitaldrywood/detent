-- +goose Up
CREATE TABLE platform.import_checkpoint (
    singleton BOOLEAN PRIMARY KEY CHECK(singleton),
    snapshot_id TEXT NOT NULL,
    catalog_digest TEXT NOT NULL,
    stage BIGINT NOT NULL DEFAULT 0 CHECK(stage >= 0),
    processed BIGINT NOT NULL DEFAULT 0 CHECK(processed >= 0),
    revision BIGINT NOT NULL DEFAULT 0 CHECK(revision >= 0),
    after_key_1 TEXT NOT NULL DEFAULT '',
    after_key_2 TEXT NOT NULL DEFAULT '',
    after_key_3 TEXT NOT NULL DEFAULT '',
    started BOOLEAN NOT NULL DEFAULT false
);
GRANT SELECT, INSERT, UPDATE, DELETE ON platform.import_checkpoint TO detent_platform;
ALTER TABLE platform.administration_receipts ADD COLUMN import_source_id BIGINT NOT NULL DEFAULT 0;
CREATE INDEX oauth_grants_import_lookup ON platform.oauth_grants(id);
GRANT UPDATE ON SEQUENCE platform.organization_events_id_seq, platform.platform_member_changes_id_seq, platform.audit_id_seq, platform.access_key_notifications_id_seq TO detent_platform;

-- +goose Down
REVOKE UPDATE ON SEQUENCE platform.organization_events_id_seq, platform.platform_member_changes_id_seq, platform.audit_id_seq, platform.access_key_notifications_id_seq FROM detent_platform;
DROP INDEX platform.oauth_grants_import_lookup;
ALTER TABLE platform.administration_receipts DROP COLUMN import_source_id;
DROP TABLE platform.import_checkpoint;
