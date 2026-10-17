-- +goose Up
ALTER TABLE runner_identities ADD COLUMN provider_reports_json TEXT NOT NULL DEFAULT '[]';

CREATE TABLE provider_reservations (
    lease_id TEXT PRIMARY KEY REFERENCES leases(lease_id),
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    pool TEXT NOT NULL,
    reservation_json TEXT NOT NULL
);

CREATE INDEX provider_reservations_pool ON provider_reservations(organization_id, pool);

-- +goose Down
DROP TABLE provider_reservations;
ALTER TABLE runner_identities DROP COLUMN provider_reports_json;
