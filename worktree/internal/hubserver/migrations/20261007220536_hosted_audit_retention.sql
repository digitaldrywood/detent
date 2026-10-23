-- +goose Up
CREATE INDEX hosted_audit_retention_idx ON hosted_audit(julianday(recorded_at));

-- +goose Down
DROP INDEX hosted_audit_retention_idx;
