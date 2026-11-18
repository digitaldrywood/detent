-- +goose Up
CREATE TABLE release_audit_checkpoints (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  id TEXT NOT NULL,
  repository TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  release_series TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_attempt TEXT NOT NULL,
  job_id TEXT NOT NULL,
  run_url TEXT NOT NULL,
  sequence bigint NOT NULL,
  status TEXT NOT NULL,
  bundle_commit TEXT NOT NULL,
  bundle_digest TEXT NOT NULL,
  allowance_invocations bigint NOT NULL,
  allowance_input bigint NOT NULL,
  allowance_output bigint NOT NULL,
  allowance_cached bigint NOT NULL,
  invocations bigint NOT NULL,
  input_tokens bigint NOT NULL,
  output_tokens bigint NOT NULL,
  cached_input_tokens bigint NOT NULL,
  digest TEXT NOT NULL,
  bytes bigint NOT NULL,
  recorded_at timestamptz NOT NULL,
  PRIMARY KEY (organization_id, project_id, id),
  UNIQUE (organization_id, project_id, run_id, run_attempt, job_id, sequence),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

CREATE TABLE release_audit_parts (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  checkpoint_id TEXT NOT NULL,
  part_index bigint NOT NULL,
  attachment_id TEXT NOT NULL,
  byte_offset bigint NOT NULL,
  bytes bigint NOT NULL,
  digest TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, checkpoint_id, part_index),
  FOREIGN KEY (organization_id, project_id, checkpoint_id) REFERENCES release_audit_checkpoints(organization_id, project_id, id),
  FOREIGN KEY (organization_id, project_id, attachment_id) REFERENCES attachments(organization_id, project_id, id)
);

CREATE INDEX release_audit_parts_attachment ON release_audit_parts(organization_id, attachment_id);


CREATE TABLE release_audit_heads (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 run_id text NOT NULL,
 run_attempt text NOT NULL,
 job_id text NOT NULL,
 checkpoint_id text NOT NULL,
 sequence bigint NOT NULL,
 PRIMARY KEY (organization_id,project_id,run_id,run_attempt,job_id),
 FOREIGN KEY (organization_id,project_id,checkpoint_id) REFERENCES release_audit_checkpoints(organization_id,project_id,id)
);
CREATE INDEX release_audit_heads_page ON release_audit_heads(organization_id,project_id,checkpoint_id);
ALTER TABLE release_audit_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE release_audit_heads FORCE ROW LEVEL SECURITY;
REVOKE ALL ON release_audit_heads FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE ON release_audit_heads TO detent_runtime;
GRANT SELECT,INSERT,UPDATE,DELETE ON release_audit_heads TO detent_platform;
CREATE POLICY tenant_isolation ON release_audit_heads TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON release_audit_heads TO detent_platform USING(true) WITH CHECK(true);

ALTER TABLE release_audit_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE release_audit_checkpoints FORCE ROW LEVEL SECURITY;
REVOKE ALL ON release_audit_checkpoints FROM PUBLIC;
GRANT SELECT, INSERT ON release_audit_checkpoints TO detent_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON release_audit_checkpoints TO detent_platform;
CREATE POLICY tenant_isolation ON release_audit_checkpoints TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON release_audit_checkpoints TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE release_audit_parts ENABLE ROW LEVEL SECURITY;
ALTER TABLE release_audit_parts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON release_audit_parts FROM PUBLIC;
GRANT SELECT, INSERT ON release_audit_parts TO detent_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON release_audit_parts TO detent_platform;
CREATE POLICY tenant_isolation ON release_audit_parts TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON release_audit_parts TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM release_audit_checkpoints) THEN
  RAISE EXCEPTION 'release audit records require an explicit export before rollback';
 END IF;
END $$;
DROP TABLE release_audit_heads;
DROP TABLE release_audit_parts;
DROP TABLE release_audit_checkpoints;
-- +goose StatementEnd
