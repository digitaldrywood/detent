-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE UNIQUE INDEX health_open_fingerprint ON health_findings(organization_id,fingerprint) WHERE resolved_at IS NULL;
CREATE INDEX health_open_finding_seek ON health_findings(organization_id,id) WHERE resolved_at IS NULL;

-- +goose Down
DROP INDEX health_open_finding_seek;
DROP INDEX health_open_fingerprint;
