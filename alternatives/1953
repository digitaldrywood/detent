-- +goose Up
CREATE TABLE ai_credit_periods (
  organization_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  period_start INTEGER NOT NULL,
  period_end INTEGER NOT NULL CHECK(period_end > period_start),
  allowance_micros INTEGER NOT NULL CHECK(allowance_micros >= 0),
  used_micros INTEGER NOT NULL DEFAULT 0 CHECK(used_micros >= 0 AND used_micros <= allowance_micros),
  PRIMARY KEY(organization_id,mode,period_start,period_end),
  FOREIGN KEY(organization_id,mode) REFERENCES ai_credit_accounts(organization_id,mode)
);
ALTER TABLE ai_credit_transactions ADD COLUMN included_amount_micros INTEGER NOT NULL DEFAULT 0 CHECK(included_amount_micros >= 0);

-- +goose Down
ALTER TABLE ai_credit_transactions DROP COLUMN included_amount_micros;
DROP TABLE ai_credit_periods;
