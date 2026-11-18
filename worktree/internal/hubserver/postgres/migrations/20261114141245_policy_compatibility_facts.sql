-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE policy_revisions ADD CONSTRAINT policy_revisions_payload_identity UNIQUE (organization_id, project_id, policy_id, payload_sha256);
ALTER TABLE change_review_policy_revisions ADD CONSTRAINT change_review_policy_revisions_payload_identity UNIQUE (organization_id, project_id, review_policy_id, policy_id, payload_sha256);

CREATE TABLE policy_revision_compatibility (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    policy_id text NOT NULL,
    payload_sha256 text NOT NULL,
    compatibility_schema smallint NOT NULL CHECK (compatibility_schema = 1),
    gate_github_pull_request boolean NOT NULL,
    gate_landing_mode text NOT NULL CHECK (length(gate_landing_mode) <= 64),
    gate_kind text NOT NULL CHECK (length(gate_kind) <= 64),
    gate_landing_command_digest text NOT NULL CHECK (length(gate_landing_command_digest) <= 256),
    gate_github_ci_digest text NOT NULL CHECK (length(gate_github_ci_digest) <= 256),
    gate_plan_review text NOT NULL CHECK (length(gate_plan_review) <= 64),
    gate_automated_review text NOT NULL CHECK (length(gate_automated_review) <= 64),
    gate_required_checks integer NOT NULL CHECK (gate_required_checks BETWEEN 0 AND 256),
    gate_validator boolean NOT NULL,
    gate_security_audit boolean NOT NULL,
    gate_human_review boolean NOT NULL,
    gate_plan_enabled boolean NOT NULL,
    PRIMARY KEY (organization_id, project_id, policy_id),
    FOREIGN KEY (organization_id, project_id, policy_id, payload_sha256) REFERENCES policy_revisions (organization_id, project_id, policy_id, payload_sha256)
);

CREATE TABLE review_policy_revision_compatibility (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    review_policy_id text NOT NULL,
    policy_id text NOT NULL,
    payload_sha256 text NOT NULL,
    compatibility_schema smallint NOT NULL CHECK (compatibility_schema = 1),
    require_review boolean NOT NULL,
    required_check_count integer NOT NULL CHECK (required_check_count BETWEEN 0 AND 256),
    PRIMARY KEY (organization_id, project_id, review_policy_id),
    FOREIGN KEY (organization_id, project_id, review_policy_id, policy_id, payload_sha256) REFERENCES change_review_policy_revisions (organization_id, project_id, review_policy_id, policy_id, payload_sha256)
);

CREATE TABLE review_policy_revision_checks (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    review_policy_id text NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 255),
    name text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 128),
    principal_id text NOT NULL CHECK (length(principal_id) BETWEEN 1 AND 128),
    workflow_id text NOT NULL CHECK (length(btrim(workflow_id)) > 0 AND length(workflow_id) <= 256),
    workflow_sha256 text NOT NULL CHECK (workflow_sha256 ~ '^[0-9a-f]{64}$'),
    source text NOT NULL CHECK (source IN ('customer', 'independent')),
    max_age_seconds bigint NOT NULL CHECK (max_age_seconds BETWEEN 60 AND 604800),
    PRIMARY KEY (organization_id, project_id, review_policy_id, ordinal),
    UNIQUE (organization_id, project_id, review_policy_id, name),
    FOREIGN KEY (organization_id, project_id, review_policy_id) REFERENCES review_policy_revision_compatibility (organization_id, project_id, review_policy_id)
);

CREATE TRIGGER policy_revision_compatibility_immutable BEFORE UPDATE OR DELETE ON policy_revision_compatibility FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
CREATE TRIGGER review_policy_revision_compatibility_immutable BEFORE UPDATE OR DELETE ON review_policy_revision_compatibility FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
CREATE TRIGGER review_policy_revision_checks_immutable BEFORE UPDATE OR DELETE ON review_policy_revision_checks FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

ALTER TABLE policy_revision_compatibility ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_revision_compatibility FORCE ROW LEVEL SECURITY;
REVOKE ALL ON policy_revision_compatibility FROM PUBLIC;
GRANT SELECT, INSERT ON policy_revision_compatibility TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON policy_revision_compatibility TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON policy_revision_compatibility TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE review_policy_revision_compatibility ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_policy_revision_compatibility FORCE ROW LEVEL SECURITY;
REVOKE ALL ON review_policy_revision_compatibility FROM PUBLIC;
GRANT SELECT, INSERT ON review_policy_revision_compatibility TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON review_policy_revision_compatibility TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON review_policy_revision_compatibility TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE review_policy_revision_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_policy_revision_checks FORCE ROW LEVEL SECURITY;
REVOKE ALL ON review_policy_revision_checks FROM PUBLIC;
GRANT SELECT, INSERT ON review_policy_revision_checks TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON review_policy_revision_checks TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON review_policy_revision_checks TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE review_policy_revision_checks;
DROP TABLE review_policy_revision_compatibility;
DROP TABLE policy_revision_compatibility;
ALTER TABLE change_review_policy_revisions DROP CONSTRAINT change_review_policy_revisions_payload_identity;
ALTER TABLE policy_revisions DROP CONSTRAINT policy_revisions_payload_identity;
