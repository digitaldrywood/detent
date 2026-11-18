-- +goose Up
CREATE TABLE hosted_runtime_publications (
 id text PRIMARY KEY CHECK (id<>''),
 published_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE hosted_runtime_revisions (
 organization_id text NOT NULL REFERENCES organizations(organization_id),
 id text NOT NULL CHECK (id<>''),
 local_published boolean NOT NULL,
 binding_id text NOT NULL, customer_owned boolean NOT NULL,
 object_key text NOT NULL, sha256 text NOT NULL,
 compressed_bytes bigint NOT NULL, content_bytes bigint NOT NULL,
 codec text NOT NULL, format_version integer NOT NULL,
 kind text NOT NULL, retention text NOT NULL,
 PRIMARY KEY (organization_id,id)
);
CREATE TABLE hosted_runtime_heads (
 organization_id text PRIMARY KEY REFERENCES organizations(organization_id),
 active_id text,
 prepared_id text NOT NULL,
 FOREIGN KEY (organization_id,active_id) REFERENCES hosted_runtime_revisions(organization_id,id),
 FOREIGN KEY (organization_id,prepared_id) REFERENCES hosted_runtime_revisions(organization_id,id)
);
CREATE TABLE platform.runtime_heads (
 issuer text PRIMARY KEY CHECK (issuer<>''),
 revision_id text NOT NULL REFERENCES hosted_runtime_publications(id),
 object_key text NOT NULL
);
ALTER TABLE hosted_runtime_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_runtime_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_runtime_revisions TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE hosted_runtime_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_runtime_heads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_runtime_heads TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_read ON hosted_runtime_heads FOR SELECT TO detent_platform USING (true);
CREATE POLICY platform_lock ON hosted_runtime_heads FOR UPDATE TO detent_platform USING (true) WITH CHECK (false);
ALTER TABLE hosted_runtime_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_runtime_publications FORCE ROW LEVEL SECURITY;
CREATE POLICY platform_publication ON hosted_runtime_publications TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY tenant_read ON hosted_runtime_publications FOR SELECT TO detent_runtime
 USING (EXISTS (SELECT 1 FROM hosted_runtime_revisions r WHERE r.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND r.id=hosted_runtime_publications.id));
GRANT SELECT,INSERT,UPDATE ON hosted_runtime_heads TO detent_runtime;
GRANT SELECT,INSERT ON hosted_runtime_revisions TO detent_runtime;
GRANT SELECT ON hosted_runtime_publications TO detent_runtime;
GRANT SELECT ON hosted_runtime_heads TO detent_platform;
GRANT UPDATE(organization_id) ON hosted_runtime_heads TO detent_platform;
GRANT SELECT,INSERT ON hosted_runtime_publications TO detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.runtime_heads TO detent_platform;
-- +goose Down
DROP TABLE platform.runtime_heads;
DROP TABLE hosted_runtime_heads;
DROP TABLE hosted_runtime_revisions;
DROP TABLE hosted_runtime_publications;
