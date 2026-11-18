-- +goose Up
ALTER TABLE native_work_item_page_progress ADD COLUMN publication_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE native_work_item_page_dirty DROP CONSTRAINT native_work_item_page_dirty_kind_check;
ALTER TABLE native_work_item_page_dirty ADD CHECK (kind IN ('item','comment','relationships'));
ALTER TABLE native_work_item_page_dirty
 ADD COLUMN after_type text NOT NULL DEFAULT '',
 ADD COLUMN after_direction text NOT NULL DEFAULT '',
 ADD COLUMN after_project text NOT NULL DEFAULT '',
 ADD COLUMN after_item text NOT NULL DEFAULT '',
 ADD COLUMN propagate boolean NOT NULL DEFAULT false,
 ADD COLUMN cleaning boolean NOT NULL DEFAULT false;
CREATE TABLE native_relationship_page_heads (
 organization_id text NOT NULL,project_id text NOT NULL,work_item_id text NOT NULL,
 generation bigint NOT NULL,source_at timestamptz NOT NULL,projected_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id)
);
CREATE TABLE native_relationship_pages (
 organization_id text NOT NULL,project_id text NOT NULL,work_item_id text NOT NULL,generation bigint NOT NULL,
 relationship_type text NOT NULL,direction text NOT NULL,related_project_id text NOT NULL,related_work_item_id text NOT NULL,
 number bigint NOT NULL,title text NOT NULL,state text NOT NULL,terminal boolean NOT NULL,dispatchable boolean NOT NULL,
 actor_principal_id text,web_url text NOT NULL,identifier text NOT NULL,closed_at timestamptz,started_at timestamptz,
 CHECK ((relationship_type='dependency' AND direction IN ('blocked_by','blocks')) OR (relationship_type='related' AND direction='related')),
 PRIMARY KEY (organization_id,project_id,work_item_id,generation,relationship_type,direction,related_project_id,related_work_item_id)
);
ALTER TABLE native_relationship_pages ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_relationship_pages FORCE ROW LEVEL SECURITY;
ALTER TABLE native_relationship_page_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_relationship_page_heads FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON native_relationship_pages,native_relationship_page_heads TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON native_relationship_pages TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
CREATE POLICY tenant_isolation ON native_relationship_page_heads TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
-- +goose StatementBegin
CREATE FUNCTION mark_relationship_page_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE organization text; first_id bigint; second_id bigint;
BEGIN
 IF TG_TABLE_NAME='issue_dependencies' THEN
  IF TG_OP='DELETE' THEN organization=OLD.organization_id;first_id=OLD.dependent_issue_id;second_id=OLD.blocker_issue_id;
  ELSE organization=NEW.organization_id;first_id=NEW.dependent_issue_id;second_id=NEW.blocker_issue_id;END IF;
 ELSE
  IF TG_OP='DELETE' THEN organization=OLD.organization_id;first_id=OLD.first_issue_id;second_id=OLD.second_issue_id;
  ELSE organization=NEW.organization_id;first_id=NEW.first_issue_id;second_id=NEW.second_issue_id;END IF;
 END IF;
 INSERT INTO native_work_item_page_dirty (organization_id,project_id,work_item_id,record_id,kind)
 SELECT i.organization_id,i.project_id,i.native_id,i.native_id,'relationships' FROM issues i WHERE i.organization_id=organization AND i.id IN (first_id,second_id)
 ON CONFLICT (organization_id,project_id,work_item_id,record_id,kind) DO UPDATE
 SET generation=DEFAULT,source_at=clock_timestamp(),after_type='',after_direction='',after_project='',after_item='',cleaning=false;
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER native_relationship_dependency_dirty AFTER INSERT OR UPDATE OR DELETE ON issue_dependencies FOR EACH ROW EXECUTE FUNCTION mark_relationship_page_dirty();
CREATE TRIGGER native_relationship_related_dirty AFTER INSERT OR UPDATE OR DELETE ON issue_relationships FOR EACH ROW EXECUTE FUNCTION mark_relationship_page_dirty();
-- +goose Down
ALTER TABLE native_work_item_page_progress DROP COLUMN publication_revision;
DROP TRIGGER native_relationship_related_dirty ON issue_relationships;
DROP TRIGGER native_relationship_dependency_dirty ON issue_dependencies;
DROP FUNCTION mark_relationship_page_dirty();
DROP TABLE native_relationship_pages,native_relationship_page_heads;
DELETE FROM native_work_item_page_dirty WHERE kind='relationships';
ALTER TABLE native_work_item_page_dirty DROP COLUMN after_type,DROP COLUMN after_direction,DROP COLUMN after_project,DROP COLUMN after_item,DROP COLUMN propagate,DROP COLUMN cleaning;
ALTER TABLE native_work_item_page_dirty DROP CONSTRAINT native_work_item_page_dirty_kind_check;
ALTER TABLE native_work_item_page_dirty ADD CHECK (kind IN ('item','comment'));
