-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE project_settings ADD COLUMN IF NOT EXISTS page_source_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS model_page_source_at timestamptz NOT NULL DEFAULT now();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION project_page_source_time() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.revision,NEW.model_revision,NEW.workflow_sha256,NEW.model_sha256) IS DISTINCT FROM (OLD.revision,OLD.model_revision,OLD.workflow_sha256,OLD.model_sha256) THEN
 NEW.page_source_at=clock_timestamp();
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE OR REPLACE TRIGGER project_page_source BEFORE UPDATE ON project_settings FOR EACH ROW EXECUTE FUNCTION project_page_source_time();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION organization_model_page_source_time() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.model_selection_revision IS DISTINCT FROM OLD.model_selection_revision THEN NEW.model_page_source_at=clock_timestamp(); END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE OR REPLACE TRIGGER organization_model_page_source BEFORE UPDATE ON organizations FOR EACH ROW EXECUTE FUNCTION organization_model_page_source_time();
CREATE TABLE IF NOT EXISTS project_page_read_model (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 revision bigint NOT NULL,
 source_generation bigint NOT NULL,
 organization_revision bigint NOT NULL,
 workflow_sha256 text NOT NULL,
 model_sha256 text NOT NULL,
 source_at timestamptz NOT NULL,
 projected_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 body text NOT NULL,
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS project_model_page_parts (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 ordinal bigint NOT NULL,
 kind text,
 name text,
 parent bigint,
 edge text,
 model text,
 effort text,
 level text,
 duration_ms bigint,
 tokens bigint,
 issue_complexity boolean,
 enabled boolean,
 preset text,
 normal_model text,
 complex_model text,
 default_level text,
 unavailable text,
 backend_kinds text[],
 fallback_order text[],
 levels_present boolean,
 stages_present boolean,
 rules_present boolean,
 disabled boolean,
 roles text[],
 efforts text[],
 assignees text[],
 authors text[],
 priorities integer[],
 include_labels text[],
 exclude_labels text[],
 field_names text[],
 field_values text[],
 PRIMARY KEY (organization_id,project_id,ordinal),
 FOREIGN KEY (organization_id,project_id) REFERENCES project_page_read_model(organization_id,project_id) ON DELETE CASCADE
);
ALTER TABLE project_page_read_model ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_page_read_model FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON project_page_read_model;
CREATE POLICY tenant_isolation ON project_page_read_model USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
GRANT SELECT,INSERT,UPDATE,DELETE ON project_page_read_model TO detent_runtime_v2;
ALTER TABLE project_model_page_parts ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_model_page_parts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON project_model_page_parts;
CREATE POLICY tenant_isolation ON project_model_page_parts USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
GRANT SELECT,INSERT,UPDATE,DELETE ON project_model_page_parts TO detent_runtime_v2;
DROP INDEX CONCURRENTLY IF EXISTS project_page_active;
CREATE INDEX CONCURRENTLY project_page_active ON projects(organization_id,id) WHERE deleted_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'project pages require explicit retained-data rollback';
END $$;
-- +goose StatementEnd
