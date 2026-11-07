-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE health_finding_projects ADD COLUMN IF NOT EXISTS page_sequence bigint NOT NULL DEFAULT 0;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION order_health_project_page() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 SELECT sequence INTO STRICT NEW.page_sequence FROM health_findings WHERE organization_id=NEW.organization_id AND id=NEW.finding_id;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE OR REPLACE TRIGGER health_project_page_order BEFORE INSERT OR UPDATE OF finding_id,organization_id ON health_finding_projects FOR EACH ROW EXECUTE FUNCTION order_health_project_page();
GRANT UPDATE ON health_finding_projects TO detent_runtime;
DROP INDEX CONCURRENTLY IF EXISTS health_project_page_order;
CREATE INDEX CONCURRENTLY health_project_page_order ON health_finding_projects(organization_id,project_id,page_sequence);
DROP INDEX CONCURRENTLY IF EXISTS health_project_page_cold;
CREATE INDEX CONCURRENTLY health_project_page_cold ON health_finding_projects(organization_id,project_id) WHERE page_sequence=0;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'health page ordering requires explicit retained-data rollback';
END $$;
-- +goose StatementEnd
