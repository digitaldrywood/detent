-- +goose Up
ALTER TABLE hosted_billing_accounts ADD COLUMN mode TEXT NOT NULL DEFAULT 'test' CHECK (mode IN ('test', 'live'));

CREATE TABLE hosted_billing_customer_intents (
  organization_id TEXT PRIMARY KEY REFERENCES organizations(id),
  account_id TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('test', 'live')),
  idempotency_key TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending', 'bound', 'conflict')),
  customer_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE hosted_billing_retired (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  account_id TEXT NOT NULL,
  customer_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  state_json TEXT NOT NULL,
  retired_at TEXT NOT NULL
);
