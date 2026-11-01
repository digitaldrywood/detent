-- +goose Up
CREATE INDEX native_attempts_running_project_idx ON native_attempts(organization_id,issue_id) WHERE status='running';
CREATE TABLE project_settings (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 require_visual_attachments boolean NOT NULL DEFAULT false,
 model_revision bigint NOT NULL DEFAULT 1,
 model_key text NOT NULL DEFAULT '',
 model_sha256 text NOT NULL DEFAULT '',
 model_bytes bigint NOT NULL DEFAULT 0,
 model_content_bytes bigint NOT NULL DEFAULT 0,
 intake text NOT NULL DEFAULT 'disabled' CHECK (intake IN ('disabled','manual')),
 projection text NOT NULL DEFAULT 'disabled' CHECK (projection IN ('disabled','summary')),
 repository_enabled boolean NOT NULL DEFAULT false,
 repository text NOT NULL DEFAULT '',
 checkout_repository text NOT NULL DEFAULT '',
 aging_enabled boolean NOT NULL DEFAULT true,
 duplicate_challenge boolean NOT NULL DEFAULT true,
 archive_completed_after_days bigint CHECK (archive_completed_after_days > 0),
 archive_cancelled_after_days bigint CHECK (archive_cancelled_after_days > 0),
 workflow_key text NOT NULL DEFAULT '',
 workflow_sha256 text NOT NULL DEFAULT '',
 workflow_bytes bigint NOT NULL DEFAULT 0,
 workflow_content_bytes bigint NOT NULL DEFAULT 0,
 PRIMARY KEY (organization_id, project_id),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id,id)
);
CREATE TABLE project_workflow_states (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 name text NOT NULL,
 terminal boolean NOT NULL,
 dispatchable boolean NOT NULL,
 operator_only boolean NOT NULL DEFAULT false,
 board_rank bigint NOT NULL,
 PRIMARY KEY (organization_id,project_id,name),
 UNIQUE (organization_id,project_id,board_rank) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id)
);
ALTER TABLE issues ADD COLUMN workflow_state_name text;
ALTER TABLE issues ADD CONSTRAINT issues_project_state_fk FOREIGN KEY(organization_id,project_id,workflow_state_name) REFERENCES project_workflow_states(organization_id,project_id,name);
CREATE INDEX issues_project_workflow_state_idx ON issues(organization_id,project_id,workflow_state_name);
CREATE TABLE project_workflow_transitions (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 source_name text NOT NULL,
 target_name text NOT NULL,
 transition_rank bigint NOT NULL,
 PRIMARY KEY(organization_id,project_id,source_name,target_name),
 FOREIGN KEY(organization_id,project_id,source_name) REFERENCES project_workflow_states(organization_id,project_id,name),
 FOREIGN KEY(organization_id,project_id,target_name) REFERENCES project_workflow_states(organization_id,project_id,name)
);
CREATE TABLE project_slugs (
 organization_id text NOT NULL,
 slug text NOT NULL CHECK (slug <> ''),
 project_id text NOT NULL,
 PRIMARY KEY (organization_id,slug),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id)
);
ALTER TABLE projects NO FORCE ROW LEVEL SECURITY;
INSERT INTO project_slugs(organization_id,slug,project_id) SELECT organization_id,slug,id FROM projects WHERE slug <> '';
INSERT INTO project_settings(organization_id,project_id) SELECT organization_id,id FROM projects;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
ALTER TABLE project_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_settings FORCE ROW LEVEL SECURITY;
ALTER TABLE project_workflow_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_workflow_states FORCE ROW LEVEL SECURITY;
ALTER TABLE project_workflow_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_workflow_transitions FORCE ROW LEVEL SECURITY;
ALTER TABLE project_slugs ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_slugs FORCE ROW LEVEL SECURITY;
REVOKE ALL ON project_settings,project_workflow_states,project_workflow_transitions,project_slugs FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_settings,project_workflow_states,project_workflow_transitions,project_slugs TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON project_settings TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON project_workflow_states TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON project_slugs TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON project_workflow_transitions TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON project_workflow_transitions TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON project_settings TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON project_workflow_states TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON project_slugs TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE project_slugs;
DROP TABLE project_workflow_transitions;
DROP INDEX issues_project_workflow_state_idx;
ALTER TABLE issues DROP CONSTRAINT issues_project_state_fk;
ALTER TABLE issues DROP COLUMN workflow_state_name;
DROP TABLE project_workflow_states;
DROP TABLE project_settings;
DROP INDEX native_attempts_running_project_idx;
