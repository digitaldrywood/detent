-- +goose Up
CREATE TABLE ai_credit_accounts (
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  mode TEXT NOT NULL CHECK(mode IN ('test','live')),
  balance_micros INTEGER NOT NULL DEFAULT 0,
  auto_enabled INTEGER NOT NULL DEFAULT 0 CHECK(auto_enabled IN (0,1)),
  threshold_cents INTEGER NOT NULL DEFAULT 0 CHECK(threshold_cents >= 0),
  price_id TEXT NOT NULL DEFAULT '',
  payment_method TEXT NOT NULL DEFAULT '',
  failure TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(organization_id,mode)
);
CREATE TABLE ai_credit_packs (
  price_id TEXT NOT NULL,
  mode TEXT NOT NULL CHECK(mode IN ('test','live')),
  usd_cents INTEGER NOT NULL CHECK(usd_cents > 0),
  PRIMARY KEY(price_id,mode)
);
CREATE TABLE ai_credit_purchases (
  purchase_key TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  mode TEXT NOT NULL CHECK(mode IN ('test','live')),
  price_id TEXT NOT NULL,
  usd_cents INTEGER NOT NULL CHECK(usd_cents > 0),
  automatic INTEGER NOT NULL CHECK(automatic IN (0,1)),
  payment_method TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','paid','failed')),
  payment_id TEXT NOT NULL DEFAULT '',
  session_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(session_json)),
  created_at INTEGER NOT NULL,
  FOREIGN KEY(organization_id,mode) REFERENCES ai_credit_accounts(organization_id,mode),
  FOREIGN KEY(price_id,mode) REFERENCES ai_credit_packs(price_id,mode)
);
CREATE UNIQUE INDEX ai_credit_auto_inflight ON ai_credit_purchases(organization_id,mode) WHERE automatic=1 AND state='pending';
CREATE UNIQUE INDEX ai_credit_payment_once ON ai_credit_purchases(mode,payment_id) WHERE payment_id<>'';
CREATE TABLE ai_credit_transactions (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  source TEXT NOT NULL,
  amount_micros INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('purchase','auto_fund','usage')),
  recorded_at INTEGER NOT NULL,
  FOREIGN KEY(organization_id,mode) REFERENCES ai_credit_accounts(organization_id,mode),
  UNIQUE(organization_id,mode,source)
);
ALTER TABLE hosted_billing_events ADD COLUMN credit_processed INTEGER NOT NULL DEFAULT 0 CHECK(credit_processed IN (0,1));
UPDATE hosted_billing_events SET credit_processed=1;

-- +goose Down
ALTER TABLE hosted_billing_events DROP COLUMN credit_processed;
DROP TABLE ai_credit_transactions;
DROP TABLE ai_credit_purchases;
DROP TABLE ai_credit_packs;
DROP TABLE ai_credit_accounts;
