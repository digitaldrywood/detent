-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION advance_imported_credit_transaction_id(imported_id bigint) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    target regclass := 'public.ai_credit_transactions_id_seq'::regclass;
BEGIN
    IF imported_id < 0 THEN
        RAISE EXCEPTION 'invalid imported credit transaction id' USING ERRCODE = '22023';
    END IF;
    IF imported_id = 0 THEN
        RETURN;
    END IF;
    PERFORM pg_advisory_xact_lock(target::oid::bigint);
    PERFORM setval(target, GREATEST(imported_id, nextval(target)));
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION advance_imported_credit_transaction_id(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION advance_imported_credit_transaction_id(bigint) TO detent_runtime;

CREATE TABLE credit_history_imports (
    organization_id text PRIMARY KEY REFERENCES organizations (organization_id),
    source_schema text NOT NULL,
    snapshot_id text NOT NULL CHECK (snapshot_id ~ '^[0-9a-f]{64}$'),
    catalog_digest text NOT NULL CHECK (catalog_digest ~ '^[0-9a-f]{64}$'),
    completed boolean NOT NULL DEFAULT false,
    account_rows bigint NOT NULL DEFAULT 0 CHECK (account_rows >= 0),
    pack_rows bigint NOT NULL DEFAULT 0 CHECK (pack_rows >= 0),
    transaction_rows bigint NOT NULL DEFAULT 0 CHECK (transaction_rows >= 0)
);

CREATE TABLE credit_history_import_rows (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    snapshot_id text NOT NULL CHECK (snapshot_id ~ '^[0-9a-f]{64}$'),
    source_table text NOT NULL,
    source_row_id bigint NOT NULL CHECK (source_row_id > 0),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id,source_table,source_row_id),
    FOREIGN KEY (organization_id) REFERENCES credit_history_imports (organization_id) ON DELETE CASCADE
);

ALTER TABLE credit_history_imports OWNER TO detent_migration;
ALTER TABLE credit_history_import_rows OWNER TO detent_migration;
REVOKE ALL ON credit_history_imports,credit_history_import_rows FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE ON credit_history_imports,credit_history_import_rows TO detent_runtime;
ALTER TABLE credit_history_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE credit_history_imports FORCE ROW LEVEL SECURITY;
ALTER TABLE credit_history_import_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE credit_history_import_rows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON credit_history_imports TO detent_runtime,detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON credit_history_import_rows TO detent_runtime,detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM credit_history_import_rows) OR EXISTS (SELECT 1 FROM credit_history_imports WHERE completed) THEN
        RAISE EXCEPTION 'credit history import receipts require an explicit export before rollback';
    END IF;
END
$$;
DROP TABLE credit_history_import_rows;
DROP TABLE credit_history_imports;
DROP FUNCTION advance_imported_credit_transaction_id(bigint);
