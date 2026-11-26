-- +goose Up
ALTER TABLE runner_update_requests
 ADD COLUMN hold_revision bigint NOT NULL DEFAULT 0 CHECK (hold_revision >= 0),
 ADD COLUMN hold_organization_revision bigint NOT NULL DEFAULT 0 CHECK (hold_organization_revision >= 0),
 ADD COLUMN organization_hold boolean NOT NULL DEFAULT false;

ALTER TABLE runner_lifecycle
 ADD COLUMN rollout_request_id text NOT NULL DEFAULT '',
 ADD COLUMN rollout_version text NOT NULL DEFAULT '',
 ADD COLUMN rollout_revision bigint NOT NULL DEFAULT 0 CHECK (rollout_revision >= 0);
ALTER TABLE runner_lifecycle_transitions
 ADD COLUMN rollout_request_id text NOT NULL DEFAULT '';

DROP INDEX runner_failed_release;
CREATE INDEX runner_failed_rollout ON runner_lifecycle(organization_id,version,rollout_revision)
 WHERE state = 'start_failed' AND consecutive_failed_starts >= 3 AND version = rollout_version AND rollout_request_id <> '';

-- +goose Down
DROP INDEX runner_failed_rollout;
CREATE INDEX runner_failed_release ON runner_lifecycle_transitions(organization_id,version) WHERE consecutive_failed_starts >= 3;
ALTER TABLE runner_lifecycle_transitions DROP COLUMN rollout_request_id;
ALTER TABLE runner_lifecycle DROP COLUMN rollout_request_id, DROP COLUMN rollout_version, DROP COLUMN rollout_revision;
ALTER TABLE runner_update_requests DROP COLUMN hold_revision, DROP COLUMN hold_organization_revision, DROP COLUMN organization_hold;
