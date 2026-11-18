-- +goose Up
CREATE TABLE native_board_items (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 source_revision bigint NOT NULL,
 source_event_sequence bigint NOT NULL,
 source_project_revision bigint NOT NULL,
 state text NOT NULL,
 archived boolean NOT NULL,
 terminal boolean NOT NULL,
 running boolean NOT NULL,
 dispatchable boolean NOT NULL,
 lane_rank integer NOT NULL,
 priority_rank integer NOT NULL,
 activity_missing boolean NOT NULL,
 activity_at timestamptz,
 identifier text NOT NULL,
 closed_at timestamptz,
 expires_at timestamptz,
 execution_fence bigint NOT NULL,
 actor_principal_id text NOT NULL,
 issue_type text NOT NULL,
 verification_state text NOT NULL,
 priority integer,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 payload_object_key text NOT NULL,
 payload_sha256 text NOT NULL,
 payload_bytes bigint NOT NULL CHECK (payload_bytes > 0),
 payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0),
 PRIMARY KEY (organization_id, project_id, work_item_id),
 UNIQUE (organization_id, work_item_id),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);
CREATE INDEX native_board_items_order ON native_board_items (organization_id, project_id, archived, lane_rank, state, priority_rank, activity_missing, (coalesce(activity_at, '-infinity'::timestamptz)) DESC, identifier);
CREATE INDEX native_board_items_state_order ON native_board_items (organization_id, project_id, archived, state, priority_rank, activity_missing, (coalesce(activity_at, '-infinity'::timestamptz)) DESC, identifier);
CREATE INDEX native_board_items_closed ON native_board_items (organization_id, project_id, (coalesce(closed_at, '-infinity'::timestamptz)) DESC, identifier);
CREATE INDEX native_board_items_actor ON native_board_items (organization_id, project_id, actor_principal_id, work_item_id);
CREATE INDEX native_board_items_running ON native_board_items (organization_id, project_id, archived, state, expires_at) WHERE running;

CREATE TABLE native_board_filter_values (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('label', 'assignee')),
 value text NOT NULL,
 PRIMARY KEY (organization_id, project_id, work_item_id, kind, value),
 FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES native_board_items (organization_id, project_id, work_item_id) ON DELETE CASCADE
);
CREATE INDEX native_board_filter_values_lookup ON native_board_filter_values (organization_id, project_id, kind, value, work_item_id);

CREATE TABLE native_board_counts (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 archived boolean NOT NULL,
 state text NOT NULL,
 total bigint NOT NULL CHECK (total >= 0),
 PRIMARY KEY (organization_id, project_id, archived, state),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE native_board_sequences (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 sequence bigint NOT NULL CHECK (sequence >= 0),
 replay_floor bigint NOT NULL CHECK (replay_floor >= 0 AND replay_floor <= sequence),
 PRIMARY KEY (organization_id, project_id),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE native_board_deltas (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 sequence bigint NOT NULL,
 work_item_id text NOT NULL,
 previous_object_key text,
 previous_sha256 text,
 previous_bytes bigint,
 previous_content_bytes bigint,
 current_object_key text,
 current_sha256 text,
 current_bytes bigint,
 current_content_bytes bigint,
 PRIMARY KEY (organization_id, project_id, sequence),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE,
 CHECK (num_nonnulls(previous_object_key, previous_sha256, previous_bytes, previous_content_bytes) IN (0,4)),
 CHECK (num_nonnulls(current_object_key, current_sha256, current_bytes, current_content_bytes) IN (0,4)),
 CHECK (previous_object_key IS NOT NULL OR current_object_key IS NOT NULL)
);

CREATE TABLE native_board_dirty (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 PRIMARY KEY (organization_id, project_id, work_item_id),
 FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);
CREATE INDEX native_board_dirty_tenant ON native_board_dirty (organization_id, work_item_id);

ALTER TABLE native_board_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_items FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_items FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_items TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_items TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE native_board_filter_values ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_filter_values FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_filter_values FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_filter_values TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_filter_values TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE native_board_counts ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_counts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_counts FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_counts TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_counts TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE native_board_sequences ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_sequences FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_sequences FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_sequences TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_sequences TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE native_board_deltas ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_deltas FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_deltas FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_deltas TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_deltas TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE native_board_dirty ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_dirty FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_dirty FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_dirty TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_dirty TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose StatementBegin
CREATE FUNCTION mark_native_board_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN
  INSERT INTO native_board_dirty (organization_id,project_id,work_item_id) VALUES (OLD.organization_id,OLD.project_id,OLD.native_id) ON CONFLICT DO NOTHING;
  RETURN OLD;
 END IF;
 INSERT INTO native_board_dirty (organization_id,project_id,work_item_id) VALUES (NEW.organization_id,NEW.project_id,NEW.native_id) ON CONFLICT DO NOTHING;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_board_issue_dirty AFTER INSERT OR UPDATE OR DELETE ON issues FOR EACH ROW EXECUTE FUNCTION mark_native_board_dirty();

CREATE TABLE native_board_access_revisions (
 organization_id text NOT NULL REFERENCES organizations (organization_id) ON DELETE CASCADE,
 subject_kind text NOT NULL CHECK (subject_kind IN ('organization', 'token', 'member')),
 subject_id text NOT NULL,
 revision bigint NOT NULL CHECK (revision >= 0),
 PRIMARY KEY (organization_id, subject_kind, subject_id)
);
ALTER TABLE native_board_access_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_access_revisions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_access_revisions FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_access_revisions TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_access_revisions TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose StatementBegin
CREATE FUNCTION advance_native_board_access_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 revision_organization text;
 revision_kind text;
 revision_subject text;
BEGIN
 IF TG_TABLE_NAME = 'hosted_project_grants' THEN
  IF TG_OP = 'DELETE' THEN
   revision_organization := OLD.organization_id;
   revision_subject := OLD.user_id;
  ELSE
   revision_organization := NEW.organization_id;
   revision_subject := NEW.user_id;
  END IF;
  revision_kind := 'member';
 ELSIF TG_TABLE_NAME = 'token_grants' THEN
  IF TG_OP = 'DELETE' THEN
   revision_organization := OLD.organization_id;
   revision_subject := OLD.token_id;
  ELSE
   revision_organization := NEW.organization_id;
   revision_subject := NEW.token_id;
  END IF;
  revision_kind := 'token';
 ELSE
  IF TG_OP = 'DELETE' THEN
   revision_organization := OLD.organization_id;
  ELSE
   revision_organization := NEW.organization_id;
  END IF;
  revision_kind := 'organization';
  revision_subject := revision_organization;
 END IF;
 INSERT INTO native_board_access_revisions (organization_id,subject_kind,subject_id,revision)
 VALUES (revision_organization,revision_kind,revision_subject,1)
 ON CONFLICT (organization_id,subject_kind,subject_id)
 DO UPDATE SET revision=native_board_access_revisions.revision+1;
 RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_board_member_grant_revision AFTER INSERT OR UPDATE OR DELETE ON hosted_project_grants FOR EACH ROW EXECUTE FUNCTION advance_native_board_access_revision();
CREATE TRIGGER native_board_token_grant_revision AFTER INSERT OR UPDATE OR DELETE ON token_grants FOR EACH ROW EXECUTE FUNCTION advance_native_board_access_revision();
CREATE TRIGGER native_board_project_revision AFTER INSERT OR DELETE OR UPDATE OF deleted_at ON projects FOR EACH ROW EXECUTE FUNCTION advance_native_board_access_revision();

-- +goose Down
DROP TRIGGER native_board_project_revision ON projects;
DROP TRIGGER native_board_token_grant_revision ON token_grants;
DROP TRIGGER native_board_member_grant_revision ON hosted_project_grants;
DROP FUNCTION advance_native_board_access_revision();
DROP TABLE native_board_access_revisions;
DROP TRIGGER native_board_issue_dirty ON issues;
DROP FUNCTION mark_native_board_dirty();
DROP TABLE native_board_dirty;
DROP TABLE native_board_deltas;
DROP TABLE native_board_sequences;
DROP TABLE native_board_counts;
DROP TABLE native_board_filter_values;
DROP TABLE native_board_items;
