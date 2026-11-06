-- +goose Up
CREATE TABLE conversation_prices (
    organization_id text NOT NULL REFERENCES organizations(organization_id),
    id bigint NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    effective_at bigint NOT NULL,
    input double precision NOT NULL CHECK(input >= 0 AND input < 'Infinity'::double precision),
    cached_input double precision NOT NULL CHECK(cached_input >= 0 AND cached_input < 'Infinity'::double precision),
    output double precision NOT NULL CHECK(output >= 0 AND output < 'Infinity'::double precision),
    PRIMARY KEY(organization_id,id),
    UNIQUE(organization_id,provider,model,effective_at)
);
CREATE TABLE conversation_usage (
    organization_id text NOT NULL REFERENCES organizations(organization_id),
    project_id text NOT NULL,
    conversation_id text NOT NULL,
    turn_id text NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    occurred_at bigint NOT NULL,
    input bigint NOT NULL CHECK(input >= 0),
    cached_input bigint NOT NULL CHECK(cached_input >= 0 AND cached_input <= input),
    output bigint NOT NULL CHECK(output >= 0),
    reasoning_output bigint NOT NULL CHECK(reasoning_output >= 0 AND reasoning_output <= output),
    outcome text NOT NULL,
    price_id bigint,
    cost_usd double precision CHECK(cost_usd >= 0 AND cost_usd < 'Infinity'::double precision),
    cache_savings_usd double precision CHECK(cache_savings_usd >= 0 AND cache_savings_usd < 'Infinity'::double precision),
    PRIMARY KEY(organization_id,turn_id),
    FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id),
    FOREIGN KEY(organization_id,conversation_id) REFERENCES conversations(organization_id,id),
    FOREIGN KEY(organization_id,price_id) REFERENCES conversation_prices(organization_id,id),
    CHECK((price_id IS NULL AND cost_usd IS NULL AND cache_savings_usd IS NULL) OR
          (price_id IS NOT NULL AND cost_usd IS NOT NULL AND cache_savings_usd IS NOT NULL))
);
CREATE INDEX conversation_usage_window_idx ON conversation_usage(organization_id,occurred_at,turn_id);
CREATE INDEX conversation_usage_project_window_idx ON conversation_usage(organization_id,project_id,occurred_at,turn_id);
ALTER TABLE conversation_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_prices FORCE ROW LEVEL SECURITY;
REVOKE ALL ON conversation_prices FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT ON conversation_prices TO detent_runtime;
CREATE POLICY tenant_isolation ON conversation_prices TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
ALTER TABLE conversation_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_usage FORCE ROW LEVEL SECURITY;
REVOKE ALL ON conversation_usage FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT ON conversation_usage TO detent_runtime;
CREATE POLICY tenant_isolation ON conversation_usage TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE conversation_usage;
DROP TABLE conversation_prices;
