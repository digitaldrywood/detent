-- +goose Up
CREATE TABLE hosted_maintenance_cursors (
    organization_id text NOT NULL REFERENCES organizations (organization_id) ON DELETE CASCADE,
    feature text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    cursor_key text NOT NULL,
    project text NOT NULL,
    kind text NOT NULL,
    cursor_id bigint NOT NULL,
    cursor_at timestamptz,
    PRIMARY KEY (organization_id, feature)
);
ALTER TABLE hosted_maintenance_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_maintenance_cursors FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_maintenance_cursors FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_maintenance_cursors TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON hosted_maintenance_cursors TO detent_runtime_v2, detent_migration_v2
USING (organization_id = current_setting('detent.organization_id', true))
WITH CHECK (organization_id = current_setting('detent.organization_id', true));

ALTER TABLE platform.organizations
    ADD COLUMN billing_reconcile_seconds integer CHECK (billing_reconcile_seconds = 0 OR billing_reconcile_seconds BETWEEN 60 AND 3600),
    ADD COLUMN billing_next_due_at timestamptz;
CREATE INDEX organizations_billing_due ON platform.organizations (billing_next_due_at, id) WHERE state = 'ready' AND billing_next_due_at IS NOT NULL;

-- +goose Down
DROP INDEX platform.organizations_billing_due;
ALTER TABLE platform.organizations DROP COLUMN billing_next_due_at, DROP COLUMN billing_reconcile_seconds;
DROP TABLE hosted_maintenance_cursors;
