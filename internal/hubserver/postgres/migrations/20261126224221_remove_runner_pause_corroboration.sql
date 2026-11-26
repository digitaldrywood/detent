-- +goose Up
DROP INDEX runner_update_pauses_release;
ALTER TABLE runner_update_pauses DROP COLUMN machine_id;
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision);

-- +goose Down
DROP INDEX runner_update_pauses_release;
ALTER TABLE runner_update_pauses ADD COLUMN machine_id text NOT NULL DEFAULT '';
ALTER TABLE runner_update_pauses NO FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_identities NO FORCE ROW LEVEL SECURITY;
UPDATE runner_update_pauses p SET machine_id=r.machine_id FROM runner_identities r
 WHERE (p.organization_id,p.runner_id)=(r.organization_id,r.runner_id);
ALTER TABLE runner_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_update_pauses FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_update_pauses ADD CHECK (machine_id<>'');
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision,machine_id);
