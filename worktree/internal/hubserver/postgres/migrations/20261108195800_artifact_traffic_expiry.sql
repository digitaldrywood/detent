-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY artifact_catalog_traffic_expiry ON artifact_catalog_traffic(organization_id,minute,project_id,service_id);

-- +goose Down
DROP INDEX CONCURRENTLY artifact_catalog_traffic_expiry;
