-- +goose Up
CREATE TABLE request_metric_imports (
 organization_id text PRIMARY KEY CHECK (btrim(organization_id) <> ''),
 source_schema text NOT NULL,
 snapshot_id text NOT NULL CHECK (snapshot_id <> ''),
 catalog_digest text NOT NULL,
 retain_at timestamptz NOT NULL,
 stage bigint NOT NULL DEFAULT 0 CHECK (stage BETWEEN 0 AND 3),
 after_time text NOT NULL DEFAULT '',
 after_route text NOT NULL DEFAULT '',
 after_kind text NOT NULL DEFAULT '',
 after_caller text NOT NULL DEFAULT '',
 after_status text NOT NULL DEFAULT '',
 processed bigint NOT NULL DEFAULT 0 CHECK (processed >= 0),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0)
);
ALTER TABLE request_metric_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE request_metric_imports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON request_metric_imports FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON request_metric_imports TO detent_runtime;
CREATE POLICY tenant_isolation ON request_metric_imports TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE request_metric_imports;
