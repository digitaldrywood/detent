-- +goose Up
ALTER TABLE github_outbound
 ADD COLUMN outcome text NOT NULL DEFAULT 'uncertain' CHECK(outcome IN ('unattempted','rejected','uncertain','committed')),
 ADD COLUMN phase text NOT NULL DEFAULT '',
 ADD COLUMN page_cursor text NOT NULL DEFAULT '',
 ADD COLUMN target_id bigint NOT NULL DEFAULT 0,
 ADD COLUMN fallback_id bigint NOT NULL DEFAULT 0,
 ADD COLUMN verification bigint NOT NULL DEFAULT 0,
 ADD COLUMN page_sequence bigint NOT NULL DEFAULT 0,
 ADD COLUMN source_updated_at timestamptz NOT NULL DEFAULT '0001-01-01T00:00:00Z';

CREATE TABLE github_outbound_observations (
 organization_id text NOT NULL,
 outbound_id bigint NOT NULL,
 kind text NOT NULL,
 source_key text NOT NULL,
 verification bigint NOT NULL,
 sequence bigint NOT NULL,
 ready boolean NOT NULL,
 PRIMARY KEY(organization_id,outbound_id,kind,source_key),
 FOREIGN KEY(organization_id,outbound_id) REFERENCES github_outbound(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX github_outbound_blocking ON github_outbound_observations(organization_id,outbound_id,verification) WHERE NOT ready;
ALTER TABLE github_outbound_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_outbound_observations FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON github_outbound_observations TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON github_outbound_observations TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON github_outbound_observations TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
DROP TABLE github_outbound_observations;
ALTER TABLE github_outbound DROP COLUMN outcome, DROP COLUMN phase, DROP COLUMN page_cursor,
 DROP COLUMN target_id, DROP COLUMN fallback_id, DROP COLUMN verification, DROP COLUMN page_sequence, DROP COLUMN source_updated_at;
