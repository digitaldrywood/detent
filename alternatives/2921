-- +goose Up
CREATE TABLE access_imports (
 organization_id text PRIMARY KEY CHECK (btrim(organization_id) <> ''),
 source_schema text NOT NULL,
 snapshot_id text NOT NULL CHECK (snapshot_id <> ''),
 catalog_digest text NOT NULL,
 stage bigint NOT NULL DEFAULT 0 CHECK (stage BETWEEN 0 AND 17),
 processed bigint NOT NULL DEFAULT 0 CHECK (processed >= 0),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 after_key_1 text NOT NULL DEFAULT '',
 after_key_2 text NOT NULL DEFAULT '',
 after_key_3 text NOT NULL DEFAULT '',
 has_after boolean NOT NULL DEFAULT false,
 done boolean NOT NULL DEFAULT false
);
ALTER TABLE access_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_imports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON access_imports FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON access_imports TO detent_runtime;
CREATE POLICY tenant_isolation ON access_imports TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE access_imports;
