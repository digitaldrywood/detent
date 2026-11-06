-- +goose Up

CREATE TABLE hosted_plans (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    id text NOT NULL,
    version bigint NOT NULL CHECK (version>0),
    name text NOT NULL DEFAULT '',
    monthly_usd_cents bigint CHECK (monthly_usd_cents>=0),
    runtime_id text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id,id,version)
);

CREATE TABLE hosted_plan_allowances (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    plan_id text NOT NULL,
    plan_version bigint NOT NULL,
    metric text NOT NULL,
    amount bigint NOT NULL,
    PRIMARY KEY (organization_id,plan_id,plan_version,metric),
    FOREIGN KEY (organization_id,plan_id,plan_version) REFERENCES hosted_plans (organization_id,id,version)
);

CREATE TABLE hosted_plan_features (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    plan_id text NOT NULL,
    plan_version bigint NOT NULL,
    feature text NOT NULL,
    PRIMARY KEY (organization_id,plan_id,plan_version,feature),
    FOREIGN KEY (organization_id,plan_id,plan_version) REFERENCES hosted_plans (organization_id,id,version)
);

CREATE TABLE hosted_plan_assignments (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    base_id text NOT NULL,
    base_version bigint NOT NULL,
    subscription_id text,
    subscription_version bigint,
    subscription_expires_at timestamptz,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
    PRIMARY KEY (organization_id),
    FOREIGN KEY (organization_id,base_id,base_version) REFERENCES hosted_plans (organization_id,id,version),
    FOREIGN KEY (organization_id,subscription_id,subscription_version) REFERENCES hosted_plans (organization_id,id,version),
    CHECK ((subscription_id IS NULL AND subscription_version IS NULL AND subscription_expires_at IS NULL) OR (subscription_id IS NOT NULL AND subscription_version IS NOT NULL AND subscription_expires_at IS NOT NULL))
);

CREATE TABLE hosted_complimentary_grants (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    id text NOT NULL,
    plan_id text NOT NULL,
    plan_version bigint NOT NULL,
    starts_at timestamptz NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz,
    PRIMARY KEY (organization_id,id),
    FOREIGN KEY (organization_id,plan_id,plan_version) REFERENCES hosted_plans (organization_id,id,version)
);

CREATE TABLE hosted_complimentary_grant_scopes (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    grant_id text NOT NULL,
    scope text NOT NULL,
    PRIMARY KEY (organization_id,grant_id,scope),
    FOREIGN KEY (organization_id,grant_id) REFERENCES hosted_complimentary_grants (organization_id,id)
);

CREATE INDEX hosted_complimentary_grants_effective
ON hosted_complimentary_grants(organization_id,starts_at,expires_at,id)
WHERE revoked_at IS NULL;

CREATE TABLE hosted_usage_windows (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    window_start timestamptz NOT NULL,
    metric text NOT NULL,
    amount bigint NOT NULL CHECK (amount>=0),
    PRIMARY KEY (organization_id,window_start,metric)
);

CREATE TABLE hosted_usage_receipts (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    receipt_id text NOT NULL,
    window_start timestamptz NOT NULL,
    metric text NOT NULL,
    amount bigint NOT NULL CHECK (amount>0),
    PRIMARY KEY (organization_id,receipt_id)
);

CREATE TABLE hosted_usage_counters (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    name text NOT NULL,
    value bigint NOT NULL,
    PRIMARY KEY (organization_id,name)
);

CREATE TABLE hosted_billing_accounts (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    account_id text NOT NULL,
    customer_id text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('test','live')),
    status text NOT NULL DEFAULT 'free',
    plan_id text NOT NULL DEFAULT '',
    plan_version bigint NOT NULL DEFAULT 0,
    paid_through timestamptz,
    access_until timestamptz,
    grace_until timestamptz,
    subscription_id text NOT NULL DEFAULT '',
    price_id text NOT NULL DEFAULT '',
    subscription_status text NOT NULL DEFAULT '',
    invoice_id text NOT NULL DEFAULT '',
    invoice_status text NOT NULL DEFAULT '',
    invoice_created_at timestamptz,
    period_start timestamptz,
    period_end timestamptz,
    trial_end timestamptz,
    cancel_at timestamptz,
    cancel_at_period_end boolean NOT NULL DEFAULT false,
    payment_hold text NOT NULL DEFAULT '',
    checkout_key text NOT NULL DEFAULT '',
    checkout_price_id text NOT NULL DEFAULT '',
    checkout_expires_at timestamptz,
    checkout_session_id text NOT NULL DEFAULT '',
    checkout_url text NOT NULL DEFAULT '',
    checkout_session_expires_at timestamptz,
    reconciled_at timestamptz,
    PRIMARY KEY (organization_id)
);

CREATE TABLE hosted_billing_prices (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    price_id text NOT NULL,
    plan_id text NOT NULL,
    plan_version bigint NOT NULL,
    PRIMARY KEY (organization_id,price_id),
    FOREIGN KEY (organization_id,plan_id,plan_version) REFERENCES hosted_plans (organization_id,id,version)
);

CREATE TABLE hosted_billing_events (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    sequence bigint GENERATED BY DEFAULT AS IDENTITY,
    event_id text NOT NULL,
    event_type text NOT NULL,
    received_at timestamptz NOT NULL,
    processed_at timestamptz,
    credit_processed boolean NOT NULL DEFAULT false,
    PRIMARY KEY (organization_id,sequence),
    UNIQUE (organization_id,event_id)
);

CREATE TABLE hosted_billing_customer_intents (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    account_id text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('test','live')),
    idempotency_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','bound','conflict')),
    customer_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id)
);

CREATE TABLE ai_credit_accounts (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    mode text NOT NULL CHECK (mode IN ('test','live')),
    balance_micros bigint NOT NULL DEFAULT 0,
    auto_enabled boolean NOT NULL DEFAULT false,
    threshold_cents bigint NOT NULL DEFAULT 0 CHECK (threshold_cents>=0),
    price_id text NOT NULL DEFAULT '',
    payment_method text NOT NULL DEFAULT '',
    failure text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id,mode)
);

CREATE TABLE ai_credit_packs (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    price_id text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('test','live')),
    usd_cents bigint NOT NULL CHECK (usd_cents>0),
    PRIMARY KEY (organization_id,price_id,mode)
);

CREATE TABLE ai_credit_purchases (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    purchase_key text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('test','live')),
    price_id text NOT NULL,
    usd_cents bigint NOT NULL CHECK (usd_cents>0),
    automatic boolean NOT NULL,
    payment_method text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','paid','failed')),
    payment_id text NOT NULL DEFAULT '',
    session_id text NOT NULL DEFAULT '',
    session_url text NOT NULL DEFAULT '',
    session_expires_at timestamptz,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id,purchase_key),
    FOREIGN KEY (organization_id,mode) REFERENCES ai_credit_accounts (organization_id,mode),
    FOREIGN KEY (organization_id,price_id,mode) REFERENCES ai_credit_packs (organization_id,price_id,mode)
);

CREATE UNIQUE INDEX ai_credit_purchases_one_automatic_pending
ON ai_credit_purchases(organization_id,mode)
WHERE automatic AND state='pending';

CREATE TABLE ai_credit_transactions (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    mode text NOT NULL,
    source text NOT NULL,
    amount_micros bigint NOT NULL,
    included_amount_micros bigint NOT NULL DEFAULT 0 CHECK (included_amount_micros>=0),
    kind text NOT NULL CHECK (kind IN ('purchase','auto_fund','usage','complimentary')),
    actor text NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id,id),
    UNIQUE (organization_id,mode,source),
    FOREIGN KEY (organization_id,mode) REFERENCES ai_credit_accounts (organization_id,mode)
);

CREATE TABLE ai_credit_reason_references (
    organization_id text NOT NULL,
    mode text NOT NULL,
    source text NOT NULL,
    binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    object_key text NOT NULL,
    sha256 text NOT NULL,
    compressed_bytes bigint NOT NULL CHECK (compressed_bytes>0),
    content_bytes bigint NOT NULL CHECK (content_bytes>=0),
    codec text NOT NULL,
    format_version integer NOT NULL CHECK (format_version>0),
    kind text NOT NULL,
    retention text NOT NULL,
    PRIMARY KEY (organization_id,mode,source),
    FOREIGN KEY (organization_id,mode,source) REFERENCES ai_credit_transactions (organization_id,mode,source) ON DELETE CASCADE
);

CREATE TABLE ai_credit_periods (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    mode text NOT NULL,
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    allowance_micros bigint NOT NULL CHECK (allowance_micros>=0),
    used_micros bigint NOT NULL CHECK (used_micros>=0),
    PRIMARY KEY (organization_id,mode,period_start,period_end),
    CHECK (period_end>period_start),
    FOREIGN KEY (organization_id,mode) REFERENCES ai_credit_accounts (organization_id,mode)
);

CREATE TABLE monthly_budget_policies (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    project_id text NOT NULL DEFAULT '',
    revision bigint NOT NULL CHECK (revision>0),
    enabled boolean NOT NULL,
    mode text NOT NULL CHECK (mode IN ('drain_issue','hard_stop')),
    luna_micros bigint CHECK (luna_micros>=0),
    runner_micros bigint CHECK (runner_micros>=0),
    sprite_micros bigint CHECK (sprite_micros>=0),
    total_micros bigint CHECK (total_micros>=0),
    actor_id text NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id,project_id,revision)
);

CREATE TABLE monthly_budget_admissions (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    issue_id bigint NOT NULL,
    admitted_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id,issue_id),
    FOREIGN KEY (organization_id,issue_id) REFERENCES issues (organization_id,id) ON DELETE CASCADE
);

CREATE TABLE monthly_budget_leases (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    lease_id text NOT NULL,
    luna_api boolean NOT NULL,
    runner_api boolean NOT NULL,
    sprite_infrastructure boolean NOT NULL,
    PRIMARY KEY (organization_id,lease_id),
    FOREIGN KEY (organization_id,lease_id) REFERENCES leases (organization_id,lease_id) ON DELETE CASCADE
);

CREATE TABLE hosted_plan_audit (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    command_id text NOT NULL,
    request_hash text NOT NULL,
    actor_id text NOT NULL,
    action text NOT NULL,
    recorded_at timestamptz NOT NULL,
    binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    UNIQUE (organization_id,command_id),
    object_key text NOT NULL,
    sha256 text NOT NULL,
    compressed_bytes bigint NOT NULL CHECK (compressed_bytes>0),
    content_bytes bigint NOT NULL CHECK (content_bytes>=0),
    codec text NOT NULL,
    format_version integer NOT NULL CHECK (format_version>0),
    kind text NOT NULL,
    retention text NOT NULL,
    PRIMARY KEY (organization_id,id)
);

CREATE TABLE hosted_billing_audit (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    actor_id text NOT NULL,
    action text NOT NULL,
    recorded_at timestamptz NOT NULL,
    binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    object_key text NOT NULL,
    sha256 text NOT NULL,
    compressed_bytes bigint NOT NULL CHECK (compressed_bytes>0),
    content_bytes bigint NOT NULL CHECK (content_bytes>=0),
    codec text NOT NULL,
    format_version integer NOT NULL CHECK (format_version>0),
    kind text NOT NULL,
    retention text NOT NULL,
    PRIMARY KEY (organization_id,id)
);

CREATE TABLE hosted_billing_retired (
    organization_id text NOT NULL REFERENCES organizations (organization_id),
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    account_id text NOT NULL,
    customer_id text NOT NULL,
    mode text NOT NULL,
    retired_at timestamptz NOT NULL,
    binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    object_key text NOT NULL,
    sha256 text NOT NULL,
    compressed_bytes bigint NOT NULL CHECK (compressed_bytes>0),
    content_bytes bigint NOT NULL CHECK (content_bytes>=0),
    codec text NOT NULL,
    format_version integer NOT NULL CHECK (format_version>0),
    kind text NOT NULL,
    retention text NOT NULL,
    PRIMARY KEY (organization_id,id)
);

CREATE INDEX hosted_billing_events_pending ON hosted_billing_events (organization_id,sequence) WHERE processed_at IS NULL;

CREATE INDEX hosted_billing_credit_events_pending ON hosted_billing_events (organization_id,sequence) WHERE NOT credit_processed;

CREATE UNIQUE INDEX ai_credit_payment_once ON ai_credit_purchases (organization_id,mode,payment_id) WHERE payment_id<>'';

CREATE INDEX ai_credit_history ON ai_credit_transactions (organization_id,mode,recorded_at DESC,id DESC);

ALTER TABLE hosted_plans OWNER TO detent_migration;
REVOKE ALL ON hosted_plans FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_plans TO detent_runtime;
ALTER TABLE hosted_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_plans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_plans TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_plan_allowances OWNER TO detent_migration;
REVOKE ALL ON hosted_plan_allowances FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_plan_allowances TO detent_runtime;
ALTER TABLE hosted_plan_allowances ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_plan_allowances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_plan_allowances TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_plan_features OWNER TO detent_migration;
REVOKE ALL ON hosted_plan_features FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_plan_features TO detent_runtime;
ALTER TABLE hosted_plan_features ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_plan_features FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_plan_features TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_plan_assignments OWNER TO detent_migration;
REVOKE ALL ON hosted_plan_assignments FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_plan_assignments TO detent_runtime;
ALTER TABLE hosted_plan_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_plan_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_plan_assignments TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_complimentary_grants OWNER TO detent_migration;
REVOKE ALL ON hosted_complimentary_grants FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_complimentary_grants TO detent_runtime;
ALTER TABLE hosted_complimentary_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_complimentary_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_complimentary_grants TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_complimentary_grant_scopes OWNER TO detent_migration;
REVOKE ALL ON hosted_complimentary_grant_scopes FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_complimentary_grant_scopes TO detent_runtime;
ALTER TABLE hosted_complimentary_grant_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_complimentary_grant_scopes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_complimentary_grant_scopes TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_usage_windows OWNER TO detent_migration;
REVOKE ALL ON hosted_usage_windows FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_usage_windows TO detent_runtime;
ALTER TABLE hosted_usage_windows ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_usage_windows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_usage_windows TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_usage_receipts OWNER TO detent_migration;
REVOKE ALL ON hosted_usage_receipts FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT ON hosted_usage_receipts TO detent_runtime;
ALTER TABLE hosted_usage_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_usage_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_usage_receipts TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_usage_counters OWNER TO detent_migration;
REVOKE ALL ON hosted_usage_counters FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_usage_counters TO detent_runtime;
ALTER TABLE hosted_usage_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_usage_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_usage_counters TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_billing_accounts OWNER TO detent_migration;
REVOKE ALL ON hosted_billing_accounts FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_billing_accounts TO detent_runtime;
ALTER TABLE hosted_billing_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_billing_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_billing_accounts TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_billing_prices OWNER TO detent_migration;
REVOKE ALL ON hosted_billing_prices FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_billing_prices TO detent_runtime;
ALTER TABLE hosted_billing_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_billing_prices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_billing_prices TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_billing_events OWNER TO detent_migration;
REVOKE ALL ON hosted_billing_events FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_billing_events TO detent_runtime;
ALTER TABLE hosted_billing_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_billing_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_billing_events TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_billing_customer_intents OWNER TO detent_migration;
REVOKE ALL ON hosted_billing_customer_intents FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_billing_customer_intents TO detent_runtime;
ALTER TABLE hosted_billing_customer_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_billing_customer_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_billing_customer_intents TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE ai_credit_accounts OWNER TO detent_migration;
REVOKE ALL ON ai_credit_accounts FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_credit_accounts TO detent_runtime;
ALTER TABLE ai_credit_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_credit_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_credit_accounts TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE ai_credit_packs OWNER TO detent_migration;
REVOKE ALL ON ai_credit_packs FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_credit_packs TO detent_runtime;
ALTER TABLE ai_credit_packs ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_credit_packs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_credit_packs TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE ai_credit_purchases OWNER TO detent_migration;
REVOKE ALL ON ai_credit_purchases FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_credit_purchases TO detent_runtime;
ALTER TABLE ai_credit_purchases ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_credit_purchases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_credit_purchases TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE ai_credit_transactions OWNER TO detent_migration;
REVOKE ALL ON ai_credit_transactions FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_credit_transactions TO detent_runtime;
ALTER TABLE ai_credit_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_credit_transactions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_credit_transactions TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE ai_credit_reason_references OWNER TO detent_migration;
REVOKE ALL ON ai_credit_reason_references FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT ON ai_credit_reason_references TO detent_runtime;
ALTER TABLE ai_credit_reason_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_credit_reason_references FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_credit_reason_references TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE ai_credit_periods OWNER TO detent_migration;
REVOKE ALL ON ai_credit_periods FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_credit_periods TO detent_runtime;
ALTER TABLE ai_credit_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_credit_periods FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_credit_periods TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE monthly_budget_policies OWNER TO detent_migration;
REVOKE ALL ON monthly_budget_policies FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON monthly_budget_policies TO detent_runtime;
ALTER TABLE monthly_budget_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE monthly_budget_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monthly_budget_policies TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE monthly_budget_admissions OWNER TO detent_migration;
REVOKE ALL ON monthly_budget_admissions FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON monthly_budget_admissions TO detent_runtime;
ALTER TABLE monthly_budget_admissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE monthly_budget_admissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monthly_budget_admissions TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE monthly_budget_leases OWNER TO detent_migration;
REVOKE ALL ON monthly_budget_leases FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON monthly_budget_leases TO detent_runtime;
ALTER TABLE monthly_budget_leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE monthly_budget_leases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monthly_budget_leases TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_plan_audit OWNER TO detent_migration;
REVOKE ALL ON hosted_plan_audit FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_plan_audit TO detent_runtime;
ALTER TABLE hosted_plan_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_plan_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_plan_audit TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_billing_audit OWNER TO detent_migration;
REVOKE ALL ON hosted_billing_audit FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_billing_audit TO detent_runtime;
ALTER TABLE hosted_billing_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_billing_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_billing_audit TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_billing_retired OWNER TO detent_migration;
REVOKE ALL ON hosted_billing_retired FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_billing_retired TO detent_runtime;
ALTER TABLE hosted_billing_retired ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_billing_retired FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hosted_billing_retired TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

GRANT USAGE ON SEQUENCE hosted_billing_events_sequence_seq TO detent_runtime;

GRANT USAGE ON SEQUENCE ai_credit_transactions_id_seq TO detent_runtime;

GRANT USAGE ON SEQUENCE hosted_plan_audit_id_seq TO detent_runtime;

GRANT USAGE ON SEQUENCE hosted_billing_audit_id_seq TO detent_runtime;

GRANT USAGE ON SEQUENCE hosted_billing_retired_id_seq TO detent_runtime;

-- +goose Down

DROP TABLE hosted_billing_retired;

DROP TABLE hosted_billing_audit;

DROP TABLE hosted_plan_audit;

DROP TABLE monthly_budget_leases;

DROP TABLE monthly_budget_admissions;

DROP TABLE monthly_budget_policies;

DROP TABLE ai_credit_periods;

DROP TABLE ai_credit_reason_references;

DROP TABLE ai_credit_transactions;

DROP TABLE ai_credit_purchases;

DROP TABLE ai_credit_packs;

DROP TABLE ai_credit_accounts;

DROP TABLE hosted_billing_customer_intents;

DROP TABLE hosted_billing_events;

DROP TABLE hosted_billing_prices;

DROP TABLE hosted_billing_accounts;

DROP TABLE hosted_usage_counters;

DROP TABLE hosted_usage_receipts;

DROP TABLE hosted_usage_windows;

DROP TABLE hosted_complimentary_grant_scopes;

DROP TABLE hosted_complimentary_grants;

DROP TABLE hosted_plan_assignments;

DROP TABLE hosted_plan_features;

DROP TABLE hosted_plan_allowances;

DROP TABLE hosted_plans;
