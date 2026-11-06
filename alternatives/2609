-- +goose Up
CREATE TABLE cost_imports (
    organization_id text PRIMARY KEY REFERENCES organizations (organization_id),
    source_schema text NOT NULL,
    snapshot_id text NOT NULL,
    catalog_digest text NOT NULL,
    phase integer NOT NULL DEFAULT 0 CHECK (phase BETWEEN 0 AND 3),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    source_rows bigint NOT NULL DEFAULT 0 CHECK (source_rows >= 0),
    current_sources bigint NOT NULL DEFAULT 0 CHECK (current_sources >= 0),
    history_rows bigint NOT NULL DEFAULT 0 CHECK (history_rows >= 0),
    has_after boolean NOT NULL DEFAULT false,
    after_key_1 text NOT NULL DEFAULT '',
    after_key_2 text NOT NULL DEFAULT '',
    after_key_3 text NOT NULL DEFAULT '',
    after_key_4 text NOT NULL DEFAULT '',
    after_key_5 text NOT NULL DEFAULT '',
    after_key_6 text NOT NULL DEFAULT '',
    after_key_7 text NOT NULL DEFAULT ''
);
CREATE TABLE cost_import_rows (
    organization_id text NOT NULL REFERENCES cost_imports (organization_id),
    source_key text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    project_id text NOT NULL,
    received_at text NOT NULL,
    source_voided boolean,
    binding_id text NOT NULL,
    customer_owned boolean NOT NULL,
    object_key text NOT NULL,
    sha256 text NOT NULL,
    compressed_bytes bigint NOT NULL CHECK (compressed_bytes > 0),
    content_bytes bigint NOT NULL CHECK (content_bytes > 0),
    codec text NOT NULL,
    format_version integer NOT NULL,
    kind text NOT NULL,
    retention text NOT NULL,
    PRIMARY KEY (organization_id, source_key, revision)
);
CREATE TABLE cost_import_heads (
    organization_id text NOT NULL,
    source_key text NOT NULL,
    revision bigint NOT NULL,
    PRIMARY KEY (organization_id, source_key),
    FOREIGN KEY (organization_id, source_key, revision) REFERENCES cost_import_rows (organization_id, source_key, revision)
);
ALTER TABLE cost_imports OWNER TO detent_migration;
ALTER TABLE cost_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_imports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON cost_imports FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_imports TO detent_runtime;
CREATE POLICY tenant_isolation ON cost_imports TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
ALTER TABLE cost_import_rows OWNER TO detent_migration;
ALTER TABLE cost_import_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_import_rows FORCE ROW LEVEL SECURITY;
REVOKE ALL ON cost_import_rows FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_import_rows TO detent_runtime;
CREATE POLICY tenant_isolation ON cost_import_rows TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
ALTER TABLE cost_import_heads OWNER TO detent_migration;
ALTER TABLE cost_import_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_import_heads FORCE ROW LEVEL SECURITY;
REVOKE ALL ON cost_import_heads FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_import_heads TO detent_runtime;
CREATE POLICY tenant_isolation ON cost_import_heads TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE cost_import_heads;
DROP TABLE cost_import_rows;
DROP TABLE cost_imports;
