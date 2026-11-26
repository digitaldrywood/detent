-- +goose Up
ALTER TABLE runner_lifecycle ADD COLUMN rollout_activation_id text NOT NULL DEFAULT '';
ALTER TABLE runner_update_pauses
 ADD COLUMN activation_id text NOT NULL DEFAULT '',
 ADD COLUMN supervisor_credential_id text NOT NULL DEFAULT '',
 ADD COLUMN supervisor_failed_starts integer NOT NULL DEFAULT 0,
 ADD COLUMN supervisor_reported_at timestamptz,
 ADD CONSTRAINT runner_update_pause_supervisor_check CHECK (
  (activation_id='' AND supervisor_credential_id='' AND supervisor_failed_starts=0 AND supervisor_reported_at IS NULL)
  OR (activation_id<>'' AND supervisor_credential_id<>'' AND (supervisor_failed_starts=0 OR supervisor_failed_starts>=3)
   AND supervisor_reported_at IS NOT NULL)
 );
DROP INDEX runner_update_pauses_release;
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision)
 WHERE supervisor_failed_starts>=3;

-- +goose Down
DROP INDEX runner_update_pauses_release;
CREATE INDEX runner_update_pauses_release ON runner_update_pauses(organization_id,version,rollout_revision);
ALTER TABLE runner_update_pauses DROP CONSTRAINT runner_update_pause_supervisor_check,
 DROP COLUMN activation_id, DROP COLUMN supervisor_credential_id, DROP COLUMN supervisor_failed_starts, DROP COLUMN supervisor_reported_at;
ALTER TABLE runner_lifecycle DROP COLUMN rollout_activation_id;
