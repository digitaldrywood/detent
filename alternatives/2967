-- +goose Up
CREATE TABLE conversation_billing_imports (
    organization_id text PRIMARY KEY REFERENCES organizations(organization_id),
    source_schema text NOT NULL,
    snapshot_id text NOT NULL,
    catalog_digest text NOT NULL,
    completed boolean NOT NULL DEFAULT false,
    price_rows bigint NOT NULL DEFAULT 0 CHECK(price_rows >= 0),
    usage_rows bigint NOT NULL DEFAULT 0 CHECK(usage_rows >= 0)
);
ALTER TABLE conversation_billing_imports OWNER TO detent_migration;
ALTER TABLE conversation_billing_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_billing_imports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON conversation_billing_imports FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE ON conversation_billing_imports TO detent_runtime;
CREATE POLICY tenant_isolation ON conversation_billing_imports TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE conversation_billing_imports;
