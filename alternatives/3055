-- +goose Up
CREATE TABLE native_work_item_pages (
 organization_id text,
 project_id text,
 work_item_id text,
 number bigint,
 revision bigint,
 profile text,
 web_url text,
 issue_type text,
 title text,
 body text,
 state text,
 reporter_status text,
 archived boolean,
 terminal boolean,
 scheduling_eligible boolean,
 priority integer,
 labels text[],
 assignees text[],
 created_at timestamptz,
 updated_at timestamptz,
 closed_at timestamptz,
 last_activity_at timestamptz,
 ignore_dependencies boolean,
 actor_kind text,
 actor_principal_id text,
 actor_via_key text,
 actor_display_name text,
 actor_agent text,
 provenance_provider text,
 provenance_external_id text,
 provenance_author_id text,
 provenance_author_display_name text,
 provenance_created_at timestamptz,
 provenance_updated_at timestamptz,
 provenance_observed_at timestamptz,
 report_url text,
 report_app_build text,
 report_browser text,
 report_os text,
 report_viewport text,
 report_captured_at text,
 verification_state text,
 verification_at timestamptz,
 verifier_kind text,
 verifier_principal_id text,
 verifier_via_key text,
 verifier_display_name text,
 verifier_agent text,
 contract_exempt boolean,
 contract_human_action text,
 contract_return_state text,
 contract_recorded_at timestamptz,
 contract_release_policy text,
 contract_release_body text,
 release_source_issue_id text,
 release_source_change_id text,
 release_source_merge_sha text,
 release_repository text,
 release_pr_number bigint,
 release_head_ref text,
 release_head_sha text,
 release_base_ref text,
 release_base_sha text,
 release_frozen_sha text,
 release_change_id text,
 release_version_id text,
 release_associated_at timestamptz,
 release_actor_kind text,
 release_actor_principal_id text,
 release_actor_via_key text,
 release_actor_display_name text,
 release_actor_agent text,
 source_url text,
 source_status text,
 has_provenance boolean NOT NULL,
 has_reportcontext boolean NOT NULL,
 has_verification boolean NOT NULL,
 has_verification_verifiedby boolean NOT NULL,
 has_issuecontract boolean NOT NULL,
 has_issuecontract_releasedelivery boolean NOT NULL,
 has_releasedelivery boolean NOT NULL,
 has_linkedsource boolean NOT NULL,
 source_generation bigint NOT NULL,
 source_at timestamptz NOT NULL,
 projected_at timestamptz NOT NULL,
 contract_section_names text[] NOT NULL,
 contract_section_values text[] NOT NULL,
 reference_providers text[] NOT NULL,
 reference_kinds text[] NOT NULL,
 reference_ids text[] NOT NULL,
 reference_urls text[] NOT NULL,
 reference_repositories text[] NOT NULL,
 reference_numbers bigint[] NOT NULL,
 CHECK (cardinality(contract_section_names)=cardinality(contract_section_values)),
 CHECK (cardinality(reference_providers)=cardinality(reference_kinds) AND cardinality(reference_providers)=cardinality(reference_ids) AND cardinality(reference_providers)=cardinality(reference_urls) AND cardinality(reference_providers)=cardinality(reference_repositories) AND cardinality(reference_providers)=cardinality(reference_numbers)),
 PRIMARY KEY (organization_id,project_id,work_item_id),
 CHECK (source_generation>0)
);

CREATE TABLE native_comment_pages (
 organization_id text,
 project_id text,
 work_item_id text,
 comment_id text,
 revision bigint,
 sequence bigint,
 body text,
 deleted boolean,
 created_at timestamptz,
 updated_at timestamptz,
 actor_kind text,
 actor_principal_id text,
 actor_via_key text,
 actor_display_name text,
 actor_agent text,
 editor_kind text,
 editor_principal_id text,
 editor_via_key text,
 editor_display_name text,
 editor_agent text,
 provenance_provider text,
 provenance_external_id text,
 provenance_author_id text,
 provenance_author_display_name text,
 provenance_created_at timestamptz,
 provenance_updated_at timestamptz,
 provenance_observed_at timestamptz,
 has_editedby boolean NOT NULL,
 has_provenance boolean NOT NULL,
 source_generation bigint NOT NULL,
 source_at timestamptz NOT NULL,
 projected_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id,comment_id),
 CHECK (source_generation>0)
);
CREATE UNIQUE INDEX native_comment_pages_order ON native_comment_pages (organization_id,project_id,work_item_id,sequence);
CREATE TABLE native_work_item_page_dirty (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 record_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('item','comment')),
 generation bigint GENERATED ALWAYS AS IDENTITY,
 source_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,project_id,work_item_id,record_id,kind)
);
CREATE INDEX native_work_item_page_dirty_order ON native_work_item_page_dirty (organization_id,generation);
CREATE TABLE native_work_item_page_progress (
 organization_id text PRIMARY KEY,
 cursor_generation bigint NOT NULL DEFAULT 0,
 examined_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
-- +goose StatementBegin
CREATE FUNCTION mark_work_item_page_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE row_organization text; row_project text; row_item text; row_record text; row_kind text;
BEGIN
 IF TG_TABLE_NAME='issues' THEN
  IF TG_OP='DELETE' THEN
   row_organization=OLD.organization_id; row_project=OLD.project_id; row_item=OLD.native_id;
  ELSE
   row_organization=NEW.organization_id; row_project=NEW.project_id; row_item=NEW.native_id;
  END IF;
  row_record=row_item; row_kind='item';
 ELSE
  IF TG_OP='DELETE' THEN
   row_organization=OLD.organization_id; row_project=OLD.project_id; row_item=OLD.work_item_id; row_record=OLD.id;
  ELSE
   row_organization=NEW.organization_id; row_project=NEW.project_id; row_item=NEW.work_item_id; row_record=NEW.id;
  END IF;
  row_kind='comment';
 END IF;
 INSERT INTO native_work_item_page_dirty (organization_id,project_id,work_item_id,record_id,kind)
 VALUES (row_organization,row_project,row_item,row_record,row_kind)
 ON CONFLICT (organization_id,project_id,work_item_id,record_id,kind)
 DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp();
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER native_work_item_page_issue_dirty AFTER INSERT OR UPDATE OR DELETE ON issues FOR EACH ROW EXECUTE FUNCTION mark_work_item_page_dirty();
CREATE TRIGGER native_work_item_page_comment_dirty AFTER INSERT OR UPDATE OR DELETE ON native_comments FOR EACH ROW EXECUTE FUNCTION mark_work_item_page_dirty();
ALTER TABLE native_work_item_pages ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_pages FORCE ROW LEVEL SECURITY;
ALTER TABLE native_comment_pages ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_comment_pages FORCE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_page_dirty ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_page_dirty FORCE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_page_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_page_progress FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON native_work_item_pages,native_comment_pages,native_work_item_page_dirty,native_work_item_page_progress TO detent_runtime_v2;
GRANT USAGE,SELECT ON SEQUENCE native_work_item_page_dirty_generation_seq TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON native_work_item_pages TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
CREATE POLICY tenant_isolation ON native_comment_pages TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
CREATE POLICY tenant_isolation ON native_work_item_page_dirty TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
CREATE POLICY tenant_isolation ON native_work_item_page_progress TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
-- +goose Down
DROP TRIGGER native_work_item_page_comment_dirty ON native_comments;
DROP TRIGGER native_work_item_page_issue_dirty ON issues;
DROP FUNCTION mark_work_item_page_dirty();
DROP TABLE native_work_item_page_progress,native_work_item_page_dirty,native_comment_pages,native_work_item_pages;
