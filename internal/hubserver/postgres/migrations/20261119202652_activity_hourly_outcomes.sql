-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE report_event_facts
    ADD COLUMN IF NOT EXISTS activity_stage text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS activity_no_change_status text NOT NULL DEFAULT '';
GRANT UPDATE (activity_stage,activity_no_change_status) ON report_event_facts TO detent_runtime,detent_platform;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_activity_hourly_finished;
CREATE INDEX CONCURRENTLY native_attempts_activity_hourly_finished
    ON native_attempts (organization_id, project_id, updated_at DESC, id DESC)
    WHERE status <> 'running';
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_hourly_version;
CREATE INDEX CONCURRENTLY landing_receipt_hourly_version
    ON change_landing_receipts (organization_id, version_id, merge_sha, base_ref, fencing_token, attempt_id);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_hourly_version;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_activity_hourly_finished;
ALTER TABLE report_event_facts
    DROP COLUMN IF EXISTS activity_no_change_status,
    DROP COLUMN IF EXISTS activity_stage;
RESET lock_timeout;
