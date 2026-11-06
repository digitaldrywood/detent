-- +goose Up
-- +goose StatementBegin
CREATE TABLE runner_audit_dispositions (
    organization_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY,
    project_id text NOT NULL,
    change_id text NOT NULL,
    review_sequence bigint NOT NULL,
    audit_session_id bigint NOT NULL,
    finding_id text NOT NULL,
    status text NOT NULL CHECK(status='false_positive'),
    actor_id text NOT NULL,
    request_key text NOT NULL,
    request_hash text NOT NULL,
    recorded_at timestamptz NOT NULL,
    payload_object_key text NOT NULL,
    payload_sha256 text NOT NULL,
    payload_bytes bigint NOT NULL,
    payload_content_bytes bigint NOT NULL,
    PRIMARY KEY(organization_id,id),
    UNIQUE(organization_id,actor_id,request_key),
    FOREIGN KEY(organization_id,change_id,review_sequence) REFERENCES change_evidence(organization_id,change_id,sequence)
);
ALTER TABLE runner_audit_dispositions ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_audit_dispositions FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_audit_dispositions TO detent_runtime, detent_platform;
GRANT USAGE ON SEQUENCE runner_audit_dispositions_id_seq TO detent_runtime, detent_platform, detent_migration;
CREATE POLICY tenant_isolation ON runner_audit_dispositions TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON runner_audit_dispositions TO detent_platform USING (true) WITH CHECK (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runner_audit_dispositions;
-- +goose StatementEnd
