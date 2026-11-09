-- +goose Up
CREATE UNIQUE INDEX health_open_fingerprint ON health_findings(organization_id,fingerprint) WHERE resolved_at IS NULL;

-- +goose Down
DROP INDEX health_open_fingerprint;
