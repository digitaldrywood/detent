-- +goose Up
CREATE TABLE native_execution_checklists (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    attempt_id text NOT NULL,
    revision text NOT NULL CHECK (revision ~ '^[a-f0-9]{64}$'),
    policy_id text NOT NULL,
    frozen_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, attempt_id, revision),
    FOREIGN KEY (organization_id, attempt_id) REFERENCES native_attempts (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX native_execution_checklists_latest_idx ON native_execution_checklists (organization_id, project_id, attempt_id, frozen_at DESC, revision DESC);

CREATE TABLE native_execution_checks (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    attempt_id text NOT NULL,
    revision text NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 1 AND 64),
    kind text NOT NULL CHECK (kind IN ('acceptance', 'verification', 'project_gate', 'status_gate', 'implementation_review', 'security_audit')),
    source_text text NOT NULL,
    check_digest text NOT NULL CHECK (check_digest = '' OR check_digest ~ '^[a-f0-9]{64}$'),
    status text NOT NULL CHECK (status IN ('unverified', 'needs_review', 'passed', 'failed', 'flaky', 'manual_approved', 'disclosed')),
    head_sha text NOT NULL DEFAULT '' CHECK (head_sha = '' OR head_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    tree_sha text NOT NULL DEFAULT '',
    receipt_id text NOT NULL DEFAULT '',
    checked_at timestamptz,
    PRIMARY KEY (organization_id, project_id, attempt_id, revision, ordinal),
    FOREIGN KEY (organization_id, project_id, attempt_id, revision)
        REFERENCES native_execution_checklists (organization_id, project_id, attempt_id, revision) ON DELETE CASCADE
);

ALTER TABLE native_execution_checklists ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_execution_checklists FORCE ROW LEVEL SECURITY;
ALTER TABLE native_execution_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_execution_checks FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_execution_checklists, native_execution_checks FROM PUBLIC;
GRANT SELECT, INSERT ON native_execution_checklists TO detent_runtime, detent_platform;
GRANT SELECT, INSERT, UPDATE ON native_execution_checks TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_execution_checklists TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON native_execution_checks TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_execution_checklists TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON native_execution_checks TO detent_platform USING (true) WITH CHECK (true);

CREATE TABLE native_execution_check_transitions (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    attempt_id text NOT NULL,
    revision text NOT NULL,
    ordinal integer NOT NULL,
    round_id text NOT NULL,
    status text NOT NULL CHECK (status IN ('passed', 'failed', 'flaky', 'manual_approved', 'disclosed')),
    head_sha text NOT NULL CHECK (head_sha = '' OR head_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    tree_sha text NOT NULL CHECK (tree_sha = '' OR tree_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    receipt_id text NOT NULL,
    checked_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, attempt_id, revision, ordinal, round_id),
    FOREIGN KEY (organization_id, project_id, attempt_id, revision, ordinal)
        REFERENCES native_execution_checks (organization_id, project_id, attempt_id, revision, ordinal) ON DELETE CASCADE
);

ALTER TABLE native_execution_check_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_execution_check_transitions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_execution_check_transitions FROM PUBLIC;
GRANT SELECT, INSERT ON native_execution_check_transitions TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_execution_check_transitions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_execution_check_transitions TO detent_platform USING (true) WITH CHECK (true);

CREATE TABLE native_execution_loop_interventions (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    attempt_id text NOT NULL,
    revision text NOT NULL,
    thread_id text NOT NULL,
    turn_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('steer', 'stop')),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^[a-f0-9]{64}$'),
    period integer NOT NULL CHECK (period BETWEEN 1 AND 4),
    repeats integer NOT NULL CHECK (repeats BETWEEN 3 AND 64),
    sequence bigint NOT NULL CHECK (sequence > 0),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, attempt_id, revision, thread_id, turn_id, sequence, action),
    FOREIGN KEY (organization_id, project_id, attempt_id, revision)
        REFERENCES native_execution_checklists (organization_id, project_id, attempt_id, revision) ON DELETE CASCADE
);

CREATE INDEX native_execution_interventions_read_idx ON native_execution_loop_interventions (organization_id, project_id, attempt_id, revision, observed_at DESC);
ALTER TABLE native_execution_loop_interventions ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_execution_loop_interventions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_execution_loop_interventions FROM PUBLIC;
GRANT SELECT, INSERT ON native_execution_loop_interventions TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_execution_loop_interventions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_execution_loop_interventions TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE native_execution_loop_interventions;
DROP TABLE native_execution_check_transitions;
DROP TABLE native_execution_checks;
DROP TABLE native_execution_checklists;
