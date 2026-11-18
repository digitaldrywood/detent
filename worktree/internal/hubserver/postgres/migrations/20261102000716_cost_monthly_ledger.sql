-- +goose Up
CREATE TABLE cost_sources (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    source_key text NOT NULL,
    project_id text NOT NULL,
    provider text NOT NULL,
    provider_account text NOT NULL,
    resource_id text NOT NULL,
    bucket text NOT NULL,
    metric text NOT NULL,
    period_start text NOT NULL,
    period_end text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    attribution_hash text NOT NULL,
    payload_hash text NOT NULL,
    amount_micros bigint,
    currency text NOT NULL,
    voided boolean NOT NULL,
    PRIMARY KEY (organization_id, source_key),
    CHECK (period_start < period_end)
);
CREATE INDEX cost_sources_overlap_idx ON cost_sources
    (organization_id, provider, provider_account, resource_id, bucket, metric, period_start, period_end)
    WHERE NOT voided;
CREATE TABLE cost_observation_revisions (
    organization_id text NOT NULL,
    source_key text NOT NULL,
    revision bigint NOT NULL,
    received_at timestamptz NOT NULL,
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
    PRIMARY KEY (organization_id, source_key, revision),
    FOREIGN KEY (organization_id, source_key) REFERENCES cost_sources (organization_id, source_key)
);
CREATE TABLE cost_monthly_totals (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    project_id text NOT NULL,
    month timestamptz NOT NULL,
    bucket text NOT NULL,
    currency text NOT NULL,
    amount_micros bigint NOT NULL,
    PRIMARY KEY (organization_id, project_id, month, bucket, currency)
);
ALTER TABLE cost_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_sources FORCE ROW LEVEL SECURITY;
REVOKE ALL ON cost_sources FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_sources TO detent_runtime;
CREATE POLICY tenant_isolation ON cost_sources TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
ALTER TABLE cost_observation_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_observation_revisions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON cost_observation_revisions FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_observation_revisions TO detent_runtime;
CREATE POLICY tenant_isolation ON cost_observation_revisions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
ALTER TABLE cost_monthly_totals ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_monthly_totals FORCE ROW LEVEL SECURITY;
REVOKE ALL ON cost_monthly_totals FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_monthly_totals TO detent_runtime;
CREATE POLICY tenant_isolation ON cost_monthly_totals TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE cost_monthly_totals;
DROP TABLE cost_observation_revisions;
DROP TABLE cost_sources;
