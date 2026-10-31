-- +goose Up
CREATE TABLE archive_storage_bindings (
 organization_id TEXT NOT NULL REFERENCES organizations(id),
 id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=0),
 backend TEXT NOT NULL CHECK(backend IN ('hosted','s3')),
 endpoint TEXT NOT NULL,
 region TEXT NOT NULL,
 bucket TEXT NOT NULL,
 prefix TEXT NOT NULL,
 credential_reference TEXT NOT NULL,
 versioned INTEGER CHECK(versioned IN (0,1)),
 PRIMARY KEY(organization_id,id),
 UNIQUE(organization_id,revision)
);
CREATE TABLE archive_deletion_receipts (
 organization_id TEXT NOT NULL REFERENCES organizations(id),
 binding_id TEXT NOT NULL,
 complete INTEGER NOT NULL CHECK(complete IN (0,1)),
 truncated INTEGER NOT NULL CHECK(truncated IN (0,1)),
 attempts INTEGER NOT NULL,
 updated_at TEXT NOT NULL,
 PRIMARY KEY(organization_id,binding_id)
);
CREATE TABLE archive_deletion_failures (
 organization_id TEXT NOT NULL,
 binding_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL,
 object_key TEXT NOT NULL,
 version_id TEXT NOT NULL,
 error_code TEXT NOT NULL,
 PRIMARY KEY(organization_id,binding_id,ordinal),
 FOREIGN KEY(organization_id,binding_id) REFERENCES archive_deletion_receipts(organization_id,binding_id)
);
-- +goose StatementBegin
CREATE TRIGGER archive_storage_binding_immutable BEFORE UPDATE ON archive_storage_bindings
BEGIN SELECT RAISE(ABORT,'immutable storage binding'); END;
-- +goose StatementEnd

-- +goose Down
CREATE TABLE archive_storage_downgrade_guard (value INTEGER CHECK(value=0));
INSERT INTO archive_storage_downgrade_guard VALUES(1);
