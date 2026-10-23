-- +goose Up
CREATE TABLE ai_credit_transactions_adjusted (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  source TEXT NOT NULL,
  amount_micros INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('purchase','auto_fund','usage','complimentary')),
  actor TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  recorded_at INTEGER NOT NULL,
  FOREIGN KEY(organization_id,mode) REFERENCES ai_credit_accounts(organization_id,mode),
  UNIQUE(organization_id,mode,source)
);
INSERT INTO ai_credit_transactions_adjusted(id,organization_id,mode,source,amount_micros,kind,recorded_at)
SELECT id,organization_id,mode,source,amount_micros,kind,recorded_at FROM ai_credit_transactions;
DROP TABLE ai_credit_transactions;
ALTER TABLE ai_credit_transactions_adjusted RENAME TO ai_credit_transactions;

-- +goose Down
CREATE TABLE ai_credit_transactions_original (
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
INSERT INTO ai_credit_transactions_original(id,organization_id,mode,source,amount_micros,kind,recorded_at)
SELECT id,organization_id,mode,source,amount_micros,kind,recorded_at FROM ai_credit_transactions;
DROP TABLE ai_credit_transactions;
ALTER TABLE ai_credit_transactions_original RENAME TO ai_credit_transactions;
