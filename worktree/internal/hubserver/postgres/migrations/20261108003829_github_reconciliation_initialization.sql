-- +goose Up
ALTER TABLE project_settings ADD COLUMN github_reconciliation_initialized boolean NOT NULL DEFAULT false;
CREATE INDEX github_reconciliation_uninitialized ON project_settings(organization_id,project_id)
 WHERE NOT github_reconciliation_initialized AND (repository<>'' OR checkout_repository<>'');

-- +goose Down
DROP INDEX github_reconciliation_uninitialized;
ALTER TABLE project_settings DROP COLUMN github_reconciliation_initialized;
