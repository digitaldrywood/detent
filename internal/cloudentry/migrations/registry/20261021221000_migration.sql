-- +goose Up
CREATE INDEX archive_storage_binding_origins ON archive_storage_bindings(organization_id, endpoint COLLATE NOCASE);

-- +goose Down
DROP INDEX archive_storage_binding_origins;
