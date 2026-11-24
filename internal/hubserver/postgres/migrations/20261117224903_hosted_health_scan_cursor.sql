-- +goose Up
CREATE TABLE hosted_health_scan_cursors (
    organization_id text PRIMARY KEY REFERENCES organizations(organization_id) ON DELETE CASCADE,
    generation bigint NOT NULL CHECK (generation > 0),
    project text NOT NULL,
    runner_name text NOT NULL,
    runner_id text NOT NULL,
    after_project text NOT NULL,
    item_project text NOT NULL,
    item_state text NOT NULL,
    item_id bigint NOT NULL,
    conversation text NOT NULL,
    question text NOT NULL,
    finding text NOT NULL,
    wait_finding text NOT NULL,
    wait_revision bigint NOT NULL,
    wait_conversation text NOT NULL,
    wait_question text NOT NULL,
    started_at timestamptz NOT NULL,
    cycle_failed boolean NOT NULL,
    project_scan_due_at timestamptz NOT NULL,
    runner_scan_due_at timestamptz NOT NULL,
    completed boolean[] NOT NULL
);
ALTER TABLE hosted_health_scan_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_health_scan_cursors FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_health_scan_cursors FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_health_scan_cursors TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON hosted_health_scan_cursors TO detent_runtime_v2, detent_migration_v2
USING (organization_id = current_setting('detent.organization_id', true))
WITH CHECK (organization_id = current_setting('detent.organization_id', true));

-- +goose Down
DROP TABLE hosted_health_scan_cursors;
