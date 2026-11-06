-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY hosted_audit_account_receipt_lookup
ON hosted_audit (organization_id, actual_actor, account_retry, id DESC)
WHERE account_kind = 'account_administration';

-- +goose Down
DROP INDEX CONCURRENTLY hosted_audit_account_receipt_lookup;
