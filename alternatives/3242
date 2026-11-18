-- +goose Up
SET LOCAL lock_timeout='5s';
ALTER TABLE change_landing_receipts ADD CONSTRAINT landing_receipt_derived_identity CHECK(issue_id IS NOT NULL AND change_id IS NOT NULL AND change_id<>'' AND fencing_token IS NOT NULL AND fencing_token>0 AND repository_identity IS NOT NULL AND project_id IS NOT NULL AND project_id<>'') NOT VALID;
ALTER TABLE change_landing_receipts VALIDATE CONSTRAINT landing_receipt_derived_identity;
ALTER TABLE change_landing_receipts ALTER COLUMN issue_id SET NOT NULL;
ALTER TABLE change_landing_receipts ALTER COLUMN change_id SET NOT NULL;
ALTER TABLE change_landing_receipts ALTER COLUMN fencing_token SET NOT NULL;
ALTER TABLE change_landing_receipts ALTER COLUMN repository_identity SET NOT NULL;
ALTER TABLE change_landing_receipts ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE change_landing_receipts ADD CONSTRAINT landing_receipt_retained_item FOREIGN KEY(organization_id,project_id,issue_id) REFERENCES work_item_identities(organization_id,project_id,id) NOT VALID;
ALTER TABLE change_landing_receipts ADD CONSTRAINT landing_receipt_project_change FOREIGN KEY(organization_id,project_id,change_id) REFERENCES change_requests(organization_id,project_id,id) NOT VALID;
ALTER TABLE change_landing_receipts VALIDATE CONSTRAINT landing_receipt_retained_item;
ALTER TABLE change_landing_receipts VALIDATE CONSTRAINT landing_receipt_project_change;
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'landing receipt identity cannot be discarded'; END; $$;
-- +goose StatementEnd
