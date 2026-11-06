-- +goose Up
CREATE TABLE team_threads (
 organization_id text NOT NULL,
 id text NOT NULL,
 scope text NOT NULL CHECK(scope IN ('general','project','issue')),
 project_id text,
 work_item_id text,
 title text NOT NULL,
 status text NOT NULL CHECK(status IN ('open','resolved')),
 creator_principal text NOT NULL,
 creator_name text NOT NULL,
 creator_client text NOT NULL,
 request_key text NOT NULL,
 input_hash text NOT NULL,
 post_count bigint NOT NULL DEFAULT 0 CHECK(post_count>=0),
 created_at text NOT NULL,
 updated_at text NOT NULL,
 PRIMARY KEY(organization_id,id),
 UNIQUE(organization_id,creator_principal,request_key),
 FOREIGN KEY(organization_id) REFERENCES organizations(organization_id),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id),
 CHECK((scope='general' AND project_id IS NULL AND work_item_id IS NULL) OR (scope='project' AND project_id IS NOT NULL AND work_item_id IS NULL) OR (scope='issue' AND project_id IS NOT NULL AND work_item_id IS NOT NULL))
);
CREATE INDEX team_threads_org_page ON team_threads(organization_id,updated_at DESC,id DESC);
CREATE INDEX team_threads_project_page ON team_threads(organization_id,project_id,updated_at DESC,id DESC);
CREATE INDEX team_threads_status_page ON team_threads(organization_id,status,updated_at DESC,id DESC);
CREATE INDEX team_threads_scope_page ON team_threads(organization_id,scope,status,updated_at DESC,id DESC);
CREATE INDEX team_threads_search ON team_threads USING gin(to_tsvector('english',title));
CREATE TABLE team_thread_posts (
 organization_id text NOT NULL,
 thread_id text NOT NULL,
 seq bigint NOT NULL CHECK(seq>0),
 author_principal text NOT NULL,
 author_name text NOT NULL,
 acting_for text NOT NULL,
 client_name text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('note','finding','question','decision')),
 body_archive_id text NOT NULL,
 body_object_key text NOT NULL,
 body_sha256 text NOT NULL,
 body_bytes bigint NOT NULL CHECK(body_bytes>0),
 body_content_bytes bigint NOT NULL CHECK(body_content_bytes>0),
 request_key text NOT NULL,
 input_hash text NOT NULL,
 created_at text NOT NULL,
 PRIMARY KEY(organization_id,thread_id,seq),
 UNIQUE(organization_id,thread_id,author_principal,request_key),
 FOREIGN KEY(organization_id,thread_id) REFERENCES team_threads(organization_id,id),
 FOREIGN KEY(organization_id,body_archive_id) REFERENCES archive_objects(organization_id,id)
);
CREATE TABLE team_thread_reads (
 organization_id text NOT NULL,
 thread_id text NOT NULL,
 principal_id text NOT NULL,
 last_seq bigint NOT NULL CHECK(last_seq>=0),
 PRIMARY KEY(organization_id,thread_id,principal_id),
 FOREIGN KEY(organization_id,thread_id) REFERENCES team_threads(organization_id,id)
);
CREATE TABLE team_thread_participants (
 organization_id text NOT NULL,
 thread_id text NOT NULL,
 principal_id text NOT NULL,
 client_name text NOT NULL,
 display_name text NOT NULL,
 PRIMARY KEY(organization_id,thread_id,principal_id,client_name),
 FOREIGN KEY(organization_id,thread_id) REFERENCES team_threads(organization_id,id)
);
CREATE TABLE team_thread_references (
 organization_id text NOT NULL,
 thread_id text NOT NULL,
 post_seq bigint NOT NULL CHECK(post_seq>=0),
 kind text NOT NULL CHECK(kind IN ('issue','change','attachment')),
 target_id text NOT NULL,
 project_id text NOT NULL,
 PRIMARY KEY(organization_id,thread_id,post_seq,kind,target_id),
 FOREIGN KEY(organization_id,thread_id) REFERENCES team_threads(organization_id,id)
);
CREATE TABLE team_thread_unread_summaries (
 organization_id text NOT NULL,
 id text NOT NULL,
 principal_id text NOT NULL,
 credential_id text NOT NULL,
 user_id text NOT NULL,
 member_principal text NOT NULL,
 key_kind text NOT NULL,
 require_credential_grant boolean NOT NULL,
 reporter_principal text NOT NULL,
 scope_hash text NOT NULL,
 requested_revision bigint NOT NULL DEFAULT 1,
 completed_revision bigint NOT NULL DEFAULT 0,
 scan_revision bigint NOT NULL DEFAULT 0,
 after_thread text NOT NULL DEFAULT '',
 through_thread text NOT NULL DEFAULT '',
 partial_count bigint NOT NULL DEFAULT 0,
 unread_count bigint,
 turn bigint NOT NULL DEFAULT 0,
 PRIMARY KEY(organization_id,id),
 UNIQUE(organization_id,principal_id),
 FOREIGN KEY(organization_id) REFERENCES organizations(organization_id)
);
CREATE INDEX team_thread_unread_pending ON team_thread_unread_summaries(organization_id,turn,id) WHERE requested_revision>completed_revision;
CREATE INDEX team_thread_unread_turn ON team_thread_unread_summaries(organization_id,turn);
CREATE TABLE team_thread_unread_projects (
 organization_id text NOT NULL,
 summary_id text NOT NULL,
 project_id text NOT NULL,
 PRIMARY KEY(organization_id,summary_id,project_id),
 FOREIGN KEY(organization_id,summary_id) REFERENCES team_thread_unread_summaries(organization_id,id)
);
-- +goose StatementBegin
DO $$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['team_threads','team_thread_posts','team_thread_reads','team_thread_participants','team_thread_references','team_thread_unread_summaries','team_thread_unread_projects'] LOOP
  EXECUTE format('ALTER TABLE %I OWNER TO detent_migration',table_name);
  EXECUTE format('REVOKE ALL ON %I FROM PUBLIC,detent_platform',table_name);
  EXECUTE format('GRANT SELECT,INSERT,UPDATE ON %I TO detent_runtime',table_name);
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',table_name);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',table_name);
  EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting(''detent.organization_id'',true),'''')) WITH CHECK (organization_id=NULLIF(current_setting(''detent.organization_id'',true),''''))',table_name);
 END LOOP;
END;
$$;
-- +goose StatementEnd
REVOKE UPDATE ON team_thread_posts,team_thread_references FROM detent_runtime;
GRANT DELETE ON team_thread_unread_projects TO detent_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'team threads require an explicit retained-data migration'; END; $$;
-- +goose StatementEnd
