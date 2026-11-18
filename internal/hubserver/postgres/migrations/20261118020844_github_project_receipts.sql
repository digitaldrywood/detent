-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh DROP CONSTRAINT github_pull_request_refresh_webhook_source,
    ADD COLUMN webhook_binding_project text NOT NULL DEFAULT '';
ALTER TABLE github_webhook_receipts ADD COLUMN binding_project text NOT NULL DEFAULT '',
    ADD COLUMN binding_generation bigint NOT NULL DEFAULT 0, ADD COLUMN binding_project_xid xid8,
    ADD COLUMN binding_repository_xid xid8, ADD COLUMN binding_checkout_xid xid8,
    DROP CONSTRAINT github_webhook_receipts_pkey,
    ADD PRIMARY KEY (organization_id,delivery_id,binding_project);
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_webhook_source
    FOREIGN KEY (organization_id,webhook_delivery_id,webhook_binding_project)
    REFERENCES github_webhook_receipts(organization_id,delivery_id,binding_project) NOT VALID;

-- +goose Down
ALTER TABLE github_pull_request_refresh DROP CONSTRAINT github_pull_request_refresh_webhook_source,
    DROP COLUMN webhook_binding_project;
ALTER TABLE github_webhook_receipts DROP CONSTRAINT github_webhook_receipts_pkey,
    ADD PRIMARY KEY (organization_id,delivery_id), DROP COLUMN binding_project, DROP COLUMN binding_generation, DROP COLUMN binding_project_xid,
    DROP COLUMN binding_repository_xid, DROP COLUMN binding_checkout_xid;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_webhook_source
    FOREIGN KEY (organization_id,webhook_delivery_id) REFERENCES github_webhook_receipts(organization_id,delivery_id) NOT VALID;
