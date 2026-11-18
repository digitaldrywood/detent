-- +goose Up
CREATE TABLE slack_integrations (
 organization_id text PRIMARY KEY REFERENCES organizations(organization_id),
 channel_name text NOT NULL, enabled boolean NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 last_success_at timestamptz, last_failure_at timestamptz,
 last_status_code integer NOT NULL DEFAULT 0 CHECK (last_status_code BETWEEN 0 AND 599),
 last_delivery_failed boolean NOT NULL DEFAULT false,
 updated_at timestamptz NOT NULL
);
CREATE TABLE health_slack_deliveries (
 organization_id text NOT NULL, id text NOT NULL CHECK (btrim(id)<>''),
 finding_id text NOT NULL, event text NOT NULL CHECK (event IN ('opened','resolved')),
 event_at timestamptz NOT NULL, queued_at timestamptz NOT NULL,
 next_attempt_at timestamptz NOT NULL, attempts bigint NOT NULL DEFAULT 0 CHECK (attempts>=0),
 outcome text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending','uncertain','committed','rejected')),
 external_receipt text NOT NULL DEFAULT '', completed_at timestamptz,
 status_code integer NOT NULL DEFAULT 0 CHECK (status_code BETWEEN 0 AND 599),
 PRIMARY KEY (organization_id,id), UNIQUE (organization_id,finding_id,event,event_at),
 FOREIGN KEY (organization_id,finding_id) REFERENCES health_findings(organization_id,id)
);
CREATE INDEX slack_delivery_pending ON health_slack_deliveries(organization_id,next_attempt_at,id) WHERE completed_at IS NULL;

ALTER TABLE slack_integrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE slack_integrations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON slack_integrations FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON slack_integrations TO detent_runtime;
CREATE POLICY tenant_isolation ON slack_integrations TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE health_slack_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE health_slack_deliveries FORCE ROW LEVEL SECURITY;
REVOKE ALL ON health_slack_deliveries FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON health_slack_deliveries TO detent_runtime;
CREATE POLICY tenant_isolation ON health_slack_deliveries TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE health_slack_deliveries;
DROP TABLE slack_integrations;
