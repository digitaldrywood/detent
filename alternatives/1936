-- +goose Up
ALTER TABLE native_attempts ADD COLUMN hot_projection_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN health_failure_signature TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN health_recovery_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN health_instance_failure INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN health_landing INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN health_landed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN health_conflict INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN health_landing_at TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN health_landing_key TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN health_updated_day REAL;
ALTER TABLE native_attempts ADD COLUMN health_landing_day REAL;
CREATE INDEX native_attempts_health_updated_idx ON native_attempts(organization_id,project_id,health_updated_day) WHERE hot_projection_version=1;
CREATE INDEX native_attempts_health_landing_idx ON native_attempts(organization_id,project_id,health_landing_day) WHERE hot_projection_version=1;
CREATE INDEX native_attempts_hot_conversion_idx ON native_attempts(id) WHERE hot_projection_version=0;
CREATE INDEX native_attempts_local_identity_idx ON native_attempts(organization_id,project_id,work_item_id,local_attempt_id) WHERE hot_projection_version=1;

ALTER TABLE workflow_states ADD COLUMN board_rank INTEGER;
CREATE INDEX workflow_states_rank_conversion_idx ON workflow_states(project_id) WHERE board_rank IS NULL;
CREATE INDEX native_attempts_hot_legacy_identity_idx ON native_attempts(organization_id,project_id,work_item_id,id) WHERE hot_projection_version=0;

-- +goose Down
DROP INDEX native_attempts_health_landing_idx;
DROP INDEX native_attempts_health_updated_idx;
ALTER TABLE native_attempts DROP COLUMN health_landing_day;
ALTER TABLE native_attempts DROP COLUMN health_updated_day;
DROP INDEX native_attempts_hot_legacy_identity_idx;
DROP INDEX workflow_states_rank_conversion_idx;
ALTER TABLE workflow_states DROP COLUMN board_rank;
DROP INDEX native_attempts_local_identity_idx;
DROP INDEX native_attempts_hot_conversion_idx;
ALTER TABLE native_attempts DROP COLUMN health_landing_key;
ALTER TABLE native_attempts DROP COLUMN health_landing_at;
ALTER TABLE native_attempts DROP COLUMN health_conflict;
ALTER TABLE native_attempts DROP COLUMN health_landed;
ALTER TABLE native_attempts DROP COLUMN health_landing;
ALTER TABLE native_attempts DROP COLUMN health_instance_failure;
ALTER TABLE native_attempts DROP COLUMN health_recovery_reason;
ALTER TABLE native_attempts DROP COLUMN health_failure_signature;
ALTER TABLE native_attempts DROP COLUMN hot_projection_version;
