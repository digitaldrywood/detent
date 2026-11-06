-- +goose Up
ALTER TABLE health_findings ADD COLUMN email_open_attempted INTEGER NOT NULL DEFAULT 0 CHECK (email_open_attempted IN (0, 1));
ALTER TABLE health_findings ADD COLUMN email_resolve_attempted INTEGER NOT NULL DEFAULT 0 CHECK (email_resolve_attempted IN (0, 1));
ALTER TABLE health_findings ADD COLUMN email_unavailable INTEGER NOT NULL DEFAULT 0 CHECK (email_unavailable IN (0, 1));

-- +goose Down
ALTER TABLE health_findings DROP COLUMN email_unavailable;
ALTER TABLE health_findings DROP COLUMN email_resolve_attempted;
ALTER TABLE health_findings DROP COLUMN email_open_attempted;
