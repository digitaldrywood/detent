-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY artifact_catalog_traffic_receipts_project_expiry ON artifact_catalog_traffic_receipts(organization_id,project_id,service_id,minute,id);

-- +goose Down
DROP INDEX CONCURRENTLY artifact_catalog_traffic_receipts_project_expiry;
