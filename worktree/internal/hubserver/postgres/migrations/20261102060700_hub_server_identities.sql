-- +goose Up
CREATE TABLE hub_server_identities (
    organization_id text PRIMARY KEY REFERENCES organizations (organization_id),
    server_id text NOT NULL CHECK (server_id ~ '^hub_[0-9a-f]{32}$'),
    cursor_key bytea NOT NULL CHECK (octet_length(cursor_key) = 32)
);
ALTER TABLE hub_server_identities OWNER TO detent_migration;
ALTER TABLE hub_server_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE hub_server_identities FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hub_server_identities FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT ON hub_server_identities TO detent_runtime;
CREATE POLICY tenant_isolation ON hub_server_identities TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE hub_server_identities;
