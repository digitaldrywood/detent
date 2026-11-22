-- +goose Up
CREATE TABLE runner_update_targets (
 organization_id text NOT NULL REFERENCES organizations(organization_id),
 runner_id text NOT NULL,
 version text NOT NULL,
 revision bigint NOT NULL CHECK(revision > 0),
 PRIMARY KEY(organization_id,runner_id)
);
ALTER TABLE runner_update_targets OWNER TO detent_migration;
REVOKE ALL ON runner_update_targets FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON runner_update_targets TO detent_runtime;
ALTER TABLE runner_update_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_update_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_update_targets TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE INDEX runner_failed_release ON runner_lifecycle_transitions(organization_id,version) WHERE consecutive_failed_starts >= 3;
-- +goose Down
DROP INDEX runner_failed_release;
DROP TABLE runner_update_targets;
