-- +goose Up
CREATE INDEX artifact_catalog_traffic_service_expiry ON artifact_catalog_traffic(organization_id,service_id,minute,project_id);
CREATE INDEX artifact_catalog_traffic_receipts_service_expiry ON artifact_catalog_traffic_receipts(organization_id,service_id,minute,project_id,id);

-- +goose Down
DROP INDEX artifact_catalog_traffic_receipts_service_expiry;
DROP INDEX artifact_catalog_traffic_service_expiry;
