-- +goose Up
ALTER TABLE provider_reservations ADD COLUMN audit_predecessor bigint
  CHECK (audit_predecessor IS NULL OR (requirement_role = 'security_audit' AND audit_predecessor >= 0));
ALTER TABLE runner_provider_report_models ADD COLUMN canonical_model text NOT NULL DEFAULT '';
ALTER TABLE provider_reservation_models ADD COLUMN canonical_model text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE provider_reservation_models DROP COLUMN canonical_model;
ALTER TABLE runner_provider_report_models DROP COLUMN canonical_model;
ALTER TABLE provider_reservations DROP COLUMN audit_predecessor;
