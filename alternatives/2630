-- +goose Up
-- +goose StatementBegin
CREATE TABLE project_onboarding (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 revision bigint NOT NULL CHECK (revision >= 0),
 repository text NOT NULL CHECK (repository IN ('', 'existing', 'generate')),
 doctor boolean NOT NULL,
 provider boolean NOT NULL,
 artifacts text NOT NULL CHECK (artifacts IN ('', 'local', 'customer')),
 updated_at text NOT NULL,
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id)
);
ALTER TABLE project_onboarding OWNER TO detent_migration;
REVOKE ALL ON project_onboarding FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE ON project_onboarding TO detent_runtime;
ALTER TABLE project_onboarding ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_onboarding FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON project_onboarding TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'project onboarding progress requires an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
