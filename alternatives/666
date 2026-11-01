-- +goose Up
CREATE TABLE entitlement_changes (
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
  action TEXT NOT NULL CHECK (action IN ('grant', 'revoke')),
  grant_id TEXT NOT NULL,
  plan_id TEXT NOT NULL,
  plan_version INTEGER NOT NULL,
  expires_at TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
  staff_email TEXT NOT NULL,
  staff_subject TEXT NOT NULL,
  recorded_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, idempotency_key)
);
CREATE INDEX entitlement_changes_grant ON entitlement_changes(organization_id, grant_id, action);

-- +goose StatementBegin
CREATE TRIGGER entitlement_change_immutable BEFORE UPDATE ON entitlement_changes
BEGIN
  SELECT RAISE(ABORT, 'entitlement change records are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER entitlement_change_delete BEFORE DELETE ON entitlement_changes
BEGIN
  SELECT RAISE(ABORT, 'entitlement change records are retained');
END;
-- +goose StatementEnd
