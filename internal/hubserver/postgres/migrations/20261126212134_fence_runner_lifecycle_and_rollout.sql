-- +goose Up
ALTER TABLE runner_lifecycle
 ADD COLUMN process_generation bigint NOT NULL DEFAULT 1 CHECK (process_generation > 0),
 ADD COLUMN accepted_history_slot smallint NOT NULL DEFAULT 63 CHECK (accepted_history_slot BETWEEN 0 AND 63),
 ADD COLUMN last_report_digest text NOT NULL DEFAULT '';
ALTER TABLE runner_lifecycle_transitions ADD COLUMN accepted_slot smallint;

ALTER TABLE runner_lifecycle_transitions NO FORCE ROW LEVEL SECURITY;
WITH ranked AS (
 SELECT organization_id,runner_id,process_id,sequence,transition_id,
 row_number() OVER (PARTITION BY organization_id,runner_id ORDER BY transition_id DESC) AS position
 FROM runner_lifecycle_transitions WHERE refusal=''
), trimmed AS (
 DELETE FROM runner_lifecycle_transitions t USING ranked r
 WHERE (t.organization_id,t.runner_id,t.process_id,t.sequence,t.transition_id)=(r.organization_id,r.runner_id,r.process_id,r.sequence,r.transition_id)
 AND r.position>64
)
UPDATE runner_lifecycle_transitions t SET accepted_slot=(64-r.position)::smallint
 FROM ranked r WHERE (t.organization_id,t.runner_id,t.process_id,t.sequence,t.transition_id)=(r.organization_id,r.runner_id,r.process_id,r.sequence,r.transition_id)
 AND r.position<=64;
ALTER TABLE runner_lifecycle_transitions FORCE ROW LEVEL SECURITY;

ALTER TABLE runner_lifecycle_transitions ADD CONSTRAINT runner_lifecycle_accepted_slot CHECK (
 (refusal='' AND accepted_slot IS NOT NULL AND accepted_slot BETWEEN 0 AND 63)
 OR (refusal<>'' AND accepted_slot IS NULL)
);
CREATE UNIQUE INDEX runner_lifecycle_accepted_history ON runner_lifecycle_transitions(organization_id,runner_id,accepted_slot) WHERE accepted_slot IS NOT NULL;

ALTER TABLE runner_update_pauses ADD COLUMN machine_id text NOT NULL DEFAULT '';
ALTER TABLE runner_update_pauses NO FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_identities NO FORCE ROW LEVEL SECURITY;
UPDATE runner_update_pauses p SET machine_id=r.machine_id FROM runner_identities r
 WHERE (p.organization_id,p.runner_id)=(r.organization_id,r.runner_id);
ALTER TABLE runner_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_update_pauses FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_update_pauses ADD CHECK (machine_id<>'');
DROP INDEX runner_update_pauses_release;
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision,machine_id);

-- +goose Down
DROP INDEX runner_update_pauses_release;
ALTER TABLE runner_update_pauses DROP COLUMN machine_id;
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision);
ALTER TABLE runner_lifecycle_transitions DROP CONSTRAINT runner_lifecycle_accepted_slot, DROP COLUMN accepted_slot;
ALTER TABLE runner_lifecycle DROP COLUMN process_generation, DROP COLUMN accepted_history_slot, DROP COLUMN last_report_digest;
