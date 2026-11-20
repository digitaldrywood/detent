-- +goose Up
SET LOCAL lock_timeout = '5s';

CREATE TABLE native_phase_receipts (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    issue_id bigint NOT NULL,
    position bigint GENERATED ALWAYS AS IDENTITY,
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    lease_id text NOT NULL,
    fencing_token bigint NOT NULL CHECK (fencing_token > 0),
    attempt_id text NOT NULL,
    phase text NOT NULL CHECK (phase ~ '^[a-z][a-z0-9_-]{0,63}$'),
    branch text NOT NULL CHECK (length(branch) BETWEEN 1 AND 512),
    head_sha text NOT NULL CHECK (head_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    tree_sha text NOT NULL CHECK (tree_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$' AND length(tree_sha) = length(head_sha)),
    policy_id text NOT NULL,
    input_digest text NOT NULL CHECK (input_digest ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (organization_id, project_id, issue_id, position),
    UNIQUE (organization_id, project_id, issue_id, fencing_token, idempotency_key),
    FOREIGN KEY (organization_id, project_id, issue_id) REFERENCES issues (organization_id, project_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, attempt_id) REFERENCES native_attempts (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX native_phase_receipts_attempt_idx ON native_phase_receipts (organization_id, attempt_id);

CREATE INDEX native_phase_receipts_source_idx
ON native_phase_receipts (organization_id, project_id, issue_id, branch, head_sha, tree_sha, policy_id, phase, input_digest, position DESC);

ALTER TABLE native_phase_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_phase_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_phase_receipts FROM PUBLIC;
GRANT SELECT, INSERT ON native_phase_receipts TO detent_runtime, detent_platform;
GRANT USAGE ON SEQUENCE native_phase_receipts_position_seq TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_phase_receipts TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_phase_receipts TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE native_attempts ADD COLUMN verdict_round_id text;
CREATE INDEX native_attempts_round_verdict_idx ON native_attempts (organization_id, project_id, issue_id, fencing_token DESC) WHERE verdict_round_id IS NOT NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP INDEX native_attempts_round_verdict_idx;
ALTER TABLE native_attempts DROP COLUMN verdict_round_id;
DROP TABLE native_phase_receipts;
