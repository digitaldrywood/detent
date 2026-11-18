-- +goose Up
CREATE TABLE conversation_history_imports (
    organization_id text PRIMARY KEY REFERENCES organizations (organization_id),
    source_schema text NOT NULL,
    snapshot_id text NOT NULL CHECK (snapshot_id ~ '^[0-9a-f]{64}$'),
    catalog_digest text NOT NULL CHECK (catalog_digest ~ '^[0-9a-f]{64}$'),
    completed boolean NOT NULL DEFAULT false,
    rows bigint NOT NULL DEFAULT 0 CHECK (rows >= 0)
);

CREATE TABLE conversation_history_import_rows (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    snapshot_id text NOT NULL CHECK (snapshot_id ~ '^[0-9a-f]{64}$'),
    source_table text NOT NULL,
    source_row_id bigint NOT NULL CHECK (source_row_id > 0),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id,source_table,source_row_id),
    FOREIGN KEY (organization_id) REFERENCES conversation_history_imports (organization_id) ON DELETE CASCADE
);

ALTER TABLE conversation_history_imports OWNER TO detent_migration;
ALTER TABLE conversation_history_import_rows OWNER TO detent_migration;
REVOKE ALL ON conversation_history_imports,conversation_history_import_rows FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE ON conversation_history_imports,conversation_history_import_rows TO detent_runtime;
ALTER TABLE conversation_history_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_history_imports FORCE ROW LEVEL SECURITY;
ALTER TABLE conversation_history_import_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_history_import_rows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversation_history_imports TO detent_runtime,detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON conversation_history_import_rows TO detent_runtime,detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM conversation_history_import_rows) OR EXISTS (SELECT 1 FROM conversation_history_imports WHERE completed) THEN
        RAISE EXCEPTION 'conversation history import receipts require an explicit export before rollback';
    END IF;
END
$$;
DROP TABLE conversation_history_import_rows;
DROP TABLE conversation_history_imports;
