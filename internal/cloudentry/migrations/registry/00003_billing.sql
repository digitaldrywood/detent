-- +goose Up
CREATE TABLE billing_customers (
  mode TEXT NOT NULL CHECK (mode IN ('test', 'live')),
  account_id TEXT NOT NULL,
  customer_id TEXT NOT NULL,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  created_at TEXT NOT NULL,
  PRIMARY KEY (mode, account_id, customer_id)
);
CREATE UNIQUE INDEX billing_customers_organization ON billing_customers(mode, account_id, organization_id);

CREATE TABLE billing_events (
  event_id TEXT PRIMARY KEY,
  mode TEXT NOT NULL CHECK (mode IN ('test', 'live')),
  event_type TEXT NOT NULL,
  customer_id TEXT NOT NULL,
  charge_id TEXT NOT NULL DEFAULT '',
  organization_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending', 'delivered', 'quarantined', 'ignored')),
  attempts INTEGER NOT NULL DEFAULT 0,
  received_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX billing_events_pending ON billing_events(status, received_at) WHERE status = 'pending';

-- +goose StatementBegin
CREATE TRIGGER billing_customer_immutable BEFORE UPDATE ON billing_customers
BEGIN
  SELECT RAISE(ABORT, 'billing customer mappings are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER billing_customer_delete BEFORE DELETE ON billing_customers
BEGIN
  SELECT RAISE(ABORT, 'billing customer mappings are retained');
END;
-- +goose StatementEnd
