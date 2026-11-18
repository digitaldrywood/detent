-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN landing_waiting boolean NOT NULL DEFAULT false;
ALTER TABLE collaboration_events ADD COLUMN landing_change_id text NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN landing_version_id text NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN landing_head_sha text NOT NULL DEFAULT '';
CREATE INDEX collaboration_events_attempt_sequence_idx ON collaboration_events (organization_id, project_id, attempt_id, sequence DESC) WHERE attempt_id <> '';

-- +goose Down
DROP INDEX collaboration_events_attempt_sequence_idx;
ALTER TABLE collaboration_events DROP COLUMN landing_head_sha;
ALTER TABLE collaboration_events DROP COLUMN landing_version_id;
ALTER TABLE collaboration_events DROP COLUMN landing_change_id;
ALTER TABLE collaboration_events DROP COLUMN landing_waiting;
