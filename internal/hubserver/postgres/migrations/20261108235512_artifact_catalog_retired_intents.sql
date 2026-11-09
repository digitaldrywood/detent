-- +goose Up
CREATE INDEX artifact_catalog_retired_intents ON artifact_catalog_manifest_intents(organization_id,project_id,service_id,upload_id,id) WHERE state IN ('committed','discarded');

-- +goose Down
DROP INDEX artifact_catalog_retired_intents;
