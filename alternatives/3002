-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE constraint_name text;
BEGIN
    FOR constraint_name IN SELECT conname FROM pg_constraint WHERE conrelid='runner_update_receipts'::regclass AND confrelid='runner_update_requests'::regclass AND contype='f'
    LOOP
        EXECUTE format('ALTER TABLE runner_update_receipts DROP CONSTRAINT %I',constraint_name);
    END LOOP;
END $$;
ALTER TABLE runner_update_receipts ADD CONSTRAINT receipt_request FOREIGN KEY(organization_id,request_id) REFERENCES runner_update_requests(organization_id,request_id);
ALTER TABLE runner_update_receipts ADD CONSTRAINT receipt_previous_request FOREIGN KEY(organization_id,previous_failure_request_id) REFERENCES runner_update_requests(organization_id,request_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runner_update_receipts DROP CONSTRAINT receipt_request;
ALTER TABLE runner_update_receipts DROP CONSTRAINT receipt_previous_request;
ALTER TABLE runner_update_receipts ADD FOREIGN KEY(organization_id,runner_id,request_id) REFERENCES runner_update_requests(organization_id,runner_id,request_id);
ALTER TABLE runner_update_receipts ADD FOREIGN KEY(organization_id,runner_id,previous_failure_request_id) REFERENCES runner_update_requests(organization_id,runner_id,request_id);
-- +goose StatementEnd
