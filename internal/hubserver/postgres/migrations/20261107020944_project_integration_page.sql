-- +goose Up
CREATE TABLE project_integration_pages (
 organization_id text NOT NULL, project_id text NOT NULL,
 name text NOT NULL,
 profile text NOT NULL,
 revision bigint NOT NULL,
 intake text NOT NULL,
 projection text NOT NULL,
 repository_enabled boolean NOT NULL,
 manual_import_enabled boolean NOT NULL,
 repository text NOT NULL,
 checkout_repository text NOT NULL,
 aging_enabled boolean NOT NULL,
 duplicate_challenge boolean NOT NULL,
 archive_completed_after_days integer,
 archive_cancelled_after_days integer,
 workflow_source text NOT NULL,
 workflow_source_revision text NOT NULL,
 workflow_markdown text NOT NULL,
 has_icon boolean NOT NULL,
 icon_kind text NOT NULL,
 icon_name text NOT NULL,
 icon_color text NOT NULL,
 icon_emoji text NOT NULL,
 icon_image_key text NOT NULL,
 icon_image_sha256 text NOT NULL,
 icon_url text NOT NULL,
 authority_keys text[] NOT NULL,
 authority_values text[] NOT NULL,
 source_generation bigint NOT NULL, source_at timestamptz NOT NULL, projected_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
CREATE TABLE project_integration_page_states (
 organization_id text NOT NULL,project_id text NOT NULL,state_position integer NOT NULL,transition_position integer NOT NULL,
 name text NOT NULL,terminal boolean NOT NULL,dispatchable boolean NOT NULL,operator_only boolean NOT NULL,target text,
 source_generation bigint NOT NULL,source_at timestamptz NOT NULL,projected_at timestamptz NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(organization_id,project_id,state_position,transition_position),
 FOREIGN KEY(organization_id,project_id) REFERENCES project_integration_pages ON DELETE CASCADE
);
CREATE TABLE project_integration_page_dirty (
 organization_id text NOT NULL,project_id text NOT NULL,generation bigint GENERATED ALWAYS AS IDENTITY,
 source_at timestamptz NOT NULL DEFAULT clock_timestamp(),attempted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,project_id),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX project_integration_page_queue ON project_integration_page_dirty(organization_id,attempted_at,project_id);
ALTER TABLE project_integration_pages ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_integration_pages FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_integration_pages TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON project_integration_pages TO detent_runtime_v2,detent_migration_v2
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE project_integration_page_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_integration_page_states FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_integration_page_states TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON project_integration_page_states TO detent_runtime_v2,detent_migration_v2
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE project_integration_page_dirty ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_integration_page_dirty FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_integration_page_dirty TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON project_integration_page_dirty TO detent_runtime_v2,detent_migration_v2
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
GRANT USAGE,SELECT ON SEQUENCE project_integration_page_dirty_generation_seq TO detent_runtime_v2;
-- +goose StatementBegin
CREATE FUNCTION mark_integration_page_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE organization text; project text;
BEGIN
 IF TG_OP='DELETE' THEN organization=OLD.organization_id; ELSE organization=NEW.organization_id; END IF;
 IF TG_TABLE_NAME='projects' THEN
  IF TG_OP='DELETE' THEN project=OLD.id; ELSE project=NEW.id; END IF;
 ELSE
  IF TG_OP='DELETE' THEN project=OLD.project_id; ELSE project=NEW.project_id; END IF;
 END IF;
 INSERT INTO project_integration_page_dirty(organization_id,project_id) SELECT organization,project WHERE EXISTS(SELECT 1 FROM projects WHERE organization_id=organization AND id=project)
 ON CONFLICT(organization_id,project_id) DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp();
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER integration_page_project_dirty AFTER INSERT OR UPDATE ON projects FOR EACH ROW EXECUTE FUNCTION mark_integration_page_dirty();
CREATE TRIGGER integration_page_settings_dirty AFTER INSERT OR UPDATE ON project_settings FOR EACH ROW EXECUTE FUNCTION mark_integration_page_dirty();
CREATE TRIGGER integration_page_states_dirty AFTER INSERT OR UPDATE OR DELETE ON project_workflow_states FOR EACH ROW EXECUTE FUNCTION mark_integration_page_dirty();
CREATE TRIGGER integration_page_transitions_dirty AFTER INSERT OR UPDATE OR DELETE ON project_workflow_transitions FOR EACH ROW EXECUTE FUNCTION mark_integration_page_dirty();
-- +goose Down
DROP TRIGGER integration_page_transitions_dirty ON project_workflow_transitions;
DROP TRIGGER integration_page_states_dirty ON project_workflow_states;
DROP TRIGGER integration_page_settings_dirty ON project_settings;
DROP TRIGGER integration_page_project_dirty ON projects;
DROP FUNCTION mark_integration_page_dirty();
DROP TABLE project_integration_page_dirty,project_integration_page_states,project_integration_pages;
