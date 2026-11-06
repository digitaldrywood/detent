-- +goose Up
CREATE TABLE runner_backend_isolations (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    backend text NOT NULL CHECK (btrim(backend) <> '' AND length(backend) <= 200),
    tiers text[] NOT NULL CHECK (cardinality(tiers) <= 2 AND tiers <@ ARRAY['sandbox','native-trusted']::text[]),
    PRIMARY KEY (organization_id,runner_id,backend),
    FOREIGN KEY (organization_id,runner_id) REFERENCES runner_identities (organization_id,runner_id) ON DELETE CASCADE
);

ALTER TABLE runner_backend_isolations OWNER TO detent_migration;
REVOKE ALL ON runner_backend_isolations FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_backend_isolations TO detent_runtime;
ALTER TABLE runner_backend_isolations ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_backend_isolations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_backend_isolations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE runner_backend_isolations;
