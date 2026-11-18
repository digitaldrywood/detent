-- +goose Up
CREATE TABLE runner_configuration_snapshots (
 organization_id text NOT NULL,runner_id text NOT NULL,revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),source_revision text NOT NULL CHECK(length(source_revision)=64),
 defaults_object_key text,defaults_sha256 text NOT NULL DEFAULT '',defaults_bytes bigint NOT NULL DEFAULT 0,defaults_content_bytes bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,runner_id),FOREIGN KEY(organization_id,runner_id) REFERENCES runner_identities(organization_id,runner_id),
 CHECK((defaults_object_key IS NULL AND defaults_sha256='' AND defaults_bytes=0 AND defaults_content_bytes=0) OR (defaults_object_key IS NOT NULL AND length(defaults_sha256)=64 AND defaults_bytes>0 AND defaults_content_bytes>0))
);
CREATE TABLE runner_project_configurations (
 organization_id text NOT NULL,runner_id text NOT NULL,project_id text NOT NULL,revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 config_revision text NOT NULL CHECK(length(config_revision)=64),policy_id text NOT NULL,state text NOT NULL CHECK(state IN ('attached','draining','detached')),paused boolean NOT NULL,allow_local_binding boolean NOT NULL,local_intake_enabled boolean NOT NULL,
 payload_object_key text NOT NULL CHECK(btrim(payload_object_key)<>''),payload_sha256 text NOT NULL CHECK(length(payload_sha256)=64),payload_bytes bigint NOT NULL CHECK(payload_bytes>0),payload_content_bytes bigint NOT NULL CHECK(payload_content_bytes>0),
 observed_config_revision text NOT NULL DEFAULT '',observed_policy_id text NOT NULL DEFAULT '',observed_registered boolean NOT NULL DEFAULT false,observed_paused boolean NOT NULL DEFAULT false,observed_draining boolean NOT NULL DEFAULT false,observed_at timestamptz,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,runner_id,project_id),FOREIGN KEY(organization_id,runner_id) REFERENCES runner_configuration_snapshots(organization_id,runner_id),FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE INDEX runner_configuration_project ON runner_project_configurations(organization_id,project_id,runner_id) WHERE state<>'detached';

ALTER TABLE runner_configuration_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_configuration_snapshots FORCE ROW LEVEL SECURITY;
REVOKE ALL ON runner_configuration_snapshots FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON runner_configuration_snapshots TO detent_runtime;
CREATE POLICY tenant_isolation ON runner_configuration_snapshots TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE runner_project_configurations ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_configurations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON runner_project_configurations FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON runner_project_configurations TO detent_runtime;
CREATE POLICY tenant_isolation ON runner_project_configurations TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE runner_project_configurations;
DROP TABLE runner_configuration_snapshots;
