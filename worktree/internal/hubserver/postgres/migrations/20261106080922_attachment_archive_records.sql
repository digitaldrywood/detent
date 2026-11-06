-- +goose Up
ALTER TABLE attachments DROP CONSTRAINT attachments_kind_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_kind_check CHECK (kind IN ('attachment','archive'));
ALTER TABLE attachments DROP CONSTRAINT attachments_size_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_size_check CHECK (size > 0 AND size <= CASE WHEN kind='archive' THEN 33554432 ELSE 20971520 END);
CREATE INDEX attachments_archive_digest ON attachments (organization_id,sha256,created_at DESC,id) WHERE kind='archive';
CREATE TABLE attachment_archive_references (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 attachment_id text NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id,attachment_id),
 FOREIGN KEY (organization_id,attachment_id) REFERENCES attachments (organization_id,id),
 FOREIGN KEY (organization_id,project_id,work_item_id) REFERENCES issues (organization_id,project_id,native_id) ON DELETE CASCADE
);
CREATE TABLE issue_archive_sections (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 archive_revision bigint NOT NULL CHECK (archive_revision>0),
 section_id text NOT NULL,
 attachment_id text NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id,archive_revision,section_id),
 FOREIGN KEY (organization_id,project_id,work_item_id,attachment_id) REFERENCES attachment_archive_references (organization_id,project_id,work_item_id,attachment_id) ON DELETE CASCADE
);
ALTER TABLE attachment_archive_references OWNER TO detent_migration;
REVOKE ALL ON attachment_archive_references FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON attachment_archive_references TO detent_runtime;
ALTER TABLE attachment_archive_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachment_archive_references FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON attachment_archive_references TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE issue_archive_sections OWNER TO detent_migration;
REVOKE ALL ON issue_archive_sections FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_archive_sections TO detent_runtime;
ALTER TABLE issue_archive_sections ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_archive_sections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON issue_archive_sections TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose StatementBegin
CREATE FUNCTION immutable_attachment_archive() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.kind='archive' THEN RAISE EXCEPTION 'immutable archive' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_attachment_archive BEFORE UPDATE ON attachments FOR EACH ROW EXECUTE FUNCTION immutable_attachment_archive();
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER immutable_attachment_archive ON attachments;
DROP FUNCTION immutable_attachment_archive();
ALTER TABLE attachments DROP CONSTRAINT attachments_size_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_size_check CHECK (size>0 AND size<=20971520);
DROP TABLE issue_archive_sections,attachment_archive_references;
DROP INDEX attachments_archive_digest;
ALTER TABLE attachments DROP CONSTRAINT attachments_kind_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_kind_check CHECK (kind='attachment');
