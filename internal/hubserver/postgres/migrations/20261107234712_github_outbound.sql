-- +goose Up
CREATE TABLE github_outbound (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 id bigint GENERATED ALWAYS AS IDENTITY,
 command_key text NOT NULL,
 repository text NOT NULL,
 issue_number bigint NOT NULL CHECK(issue_number>0),
 kind text NOT NULL,
 target_node_id text NOT NULL DEFAULT '',
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','processing','retrying','completed')),
 attempts bigint NOT NULL DEFAULT 0,
 lease_id text NOT NULL DEFAULT '',
 next_attempt_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL,
 payload_object_key text NOT NULL,
 payload_sha256 text NOT NULL,
 payload_bytes bigint NOT NULL,
 payload_content_bytes bigint NOT NULL,
 PRIMARY KEY(organization_id,id),
 UNIQUE(organization_id,command_key),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE INDEX github_outbound_ready ON github_outbound(organization_id,next_attempt_at,id) WHERE status<>'completed';
ALTER TABLE github_outbound ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_outbound FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON github_outbound TO detent_runtime,detent_platform;
GRANT USAGE ON SEQUENCE github_outbound_id_seq TO detent_runtime,detent_platform,detent_migration;
CREATE POLICY tenant_isolation ON github_outbound TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON github_outbound TO detent_platform USING(true) WITH CHECK(true);


-- +goose Down
DROP TABLE github_outbound;
