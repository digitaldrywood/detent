-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE hub_detail_objects (
 organization_id text NOT NULL REFERENCES organizations(organization_id) ON DELETE CASCADE,
 object_key text NOT NULL CHECK (object_key<>''),
 sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
 compressed_bytes bigint NOT NULL CHECK (compressed_bytes>0 AND compressed_bytes<=33554432),
 content_bytes bigint NOT NULL CHECK (content_bytes>0 AND content_bytes<=33554432),
 content bytea NOT NULL CHECK (octet_length(content)=compressed_bytes),
 created_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,object_key)
);
ALTER TABLE hub_detail_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE hub_detail_objects FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON hub_detail_objects TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON hub_detail_objects TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON hub_detail_objects TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE hub_detail_objects;
