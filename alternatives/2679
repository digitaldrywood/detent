-- +goose Up
-- +goose StatementBegin
CREATE TABLE onboarding_issue_intake (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 id text NOT NULL CHECK (id <> ''),
 revision bigint NOT NULL CHECK (revision > 0),
 batch_status text NOT NULL CHECK (batch_status <> ''),
 hub_owned boolean NOT NULL,
 payload_object_key text NOT NULL CHECK (payload_object_key <> ''),
 payload_sha256 text NOT NULL CHECK (length(payload_sha256) = 64),
 payload_bytes bigint NOT NULL CHECK (payload_bytes > 0),
 payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0),
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id)
);
ALTER TABLE onboarding_issue_intake OWNER TO detent_migration;
REVOKE ALL ON onboarding_issue_intake FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE ON onboarding_issue_intake TO detent_runtime;
ALTER TABLE onboarding_issue_intake ENABLE ROW LEVEL SECURITY;
ALTER TABLE onboarding_issue_intake FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON onboarding_issue_intake TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'project intake batches require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
