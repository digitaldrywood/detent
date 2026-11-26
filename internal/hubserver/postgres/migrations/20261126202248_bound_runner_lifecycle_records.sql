-- +goose Up
ALTER TABLE runner_lifecycle ADD COLUMN refused_through_sequence bigint NOT NULL DEFAULT 0 CHECK (refused_through_sequence >= 0);
ALTER TABLE runner_lifecycle_transitions ADD COLUMN refusal_slot smallint CHECK (refusal_slot IS NULL OR (refusal_slot BETWEEN 0 AND 63 AND refusal <> ''));
CREATE UNIQUE INDEX runner_lifecycle_refusal_slot ON runner_lifecycle_transitions(organization_id,runner_id,refusal_slot) WHERE refusal_slot IS NOT NULL;

CREATE TABLE runner_update_pauses (
 organization_id text NOT NULL,
 runner_id text NOT NULL,
 version text NOT NULL,
 rollout_revision bigint NOT NULL CHECK (rollout_revision >= 0),
 PRIMARY KEY(organization_id,runner_id,version),
 FOREIGN KEY(organization_id,runner_id) REFERENCES runner_identities(organization_id,runner_id) ON DELETE CASCADE
);
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision);
ALTER TABLE runner_lifecycle NO FORCE ROW LEVEL SECURITY;
INSERT INTO runner_update_pauses(organization_id,runner_id,version,rollout_revision)
 SELECT organization_id,runner_id,version,rollout_revision FROM runner_lifecycle
 WHERE state='start_failed' AND consecutive_failed_starts>=3 AND version=rollout_version AND rollout_request_id<>'';
ALTER TABLE runner_lifecycle FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_update_pauses OWNER TO detent_migration;
REVOKE ALL ON runner_update_pauses FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON runner_update_pauses TO detent_runtime;
ALTER TABLE runner_update_pauses ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_update_pauses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_update_pauses TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
DROP INDEX runner_failed_rollout;

ALTER TABLE runner_update_requests
 ADD COLUMN automatic_offer_slot smallint,
 ADD COLUMN automatic_organization_revision bigint NOT NULL DEFAULT 0,
 ADD COLUMN automatic_runner_revision bigint NOT NULL DEFAULT 0,
 ADD CONSTRAINT runner_update_automatic_offer CHECK (
  automatic_offer_slot IS NULL OR (automatic_offer_slot BETWEEN 0 AND 7 AND
   automatic_organization_revision >= 0 AND automatic_runner_revision >= 0 AND runner_id IS NOT NULL AND NOT urgent AND NOT follow_hub)
 );
CREATE UNIQUE INDEX runner_update_automatic_offer ON runner_update_requests(
 organization_id,runner_id,version,automatic_organization_revision,automatic_runner_revision,automatic_offer_slot
) WHERE automatic_offer_slot IS NOT NULL;

-- +goose Down
DROP INDEX runner_update_automatic_offer;
ALTER TABLE runner_update_requests DROP CONSTRAINT runner_update_automatic_offer,
 DROP COLUMN automatic_offer_slot, DROP COLUMN automatic_organization_revision, DROP COLUMN automatic_runner_revision;
CREATE INDEX runner_failed_rollout ON runner_lifecycle(organization_id,version,rollout_revision)
 WHERE state='start_failed' AND consecutive_failed_starts>=3 AND version=rollout_version AND rollout_request_id<>'';
DROP TABLE runner_update_pauses;
ALTER TABLE runner_lifecycle_transitions DROP COLUMN refusal_slot;
ALTER TABLE runner_lifecycle DROP COLUMN refused_through_sequence;
