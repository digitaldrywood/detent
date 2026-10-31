-- +goose Up
ALTER TABLE change_landing_receipts ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE change_landing_receipts ADD COLUMN conversion_state INTEGER NOT NULL DEFAULT 0;
CREATE INDEX change_landing_receipts_payload_pending ON change_landing_receipts(payload_object_key) WHERE payload_object_key='' AND record_json<>'{}';

-- +goose Down
CREATE TABLE landing_receipt_rollback_guard(value INTEGER CHECK(value=0));
INSERT INTO landing_receipt_rollback_guard SELECT EXISTS(SELECT 1 FROM change_landing_receipts WHERE payload_object_key<>'');
DROP TABLE landing_receipt_rollback_guard;
DROP INDEX change_landing_receipts_payload_pending;
ALTER TABLE change_landing_receipts DROP COLUMN conversion_state;
ALTER TABLE change_landing_receipts DROP COLUMN payload_object_key;
