-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts
    ADD COLUMN verdict_evidence_revision text,
    ADD COLUMN verdict_attempted_step text,
    ADD COLUMN verdict_success boolean,
    ADD COLUMN verdict_outcome text,
    ADD COLUMN verdict_reason text,
    ADD COLUMN verdict_next_step text,
    ADD COLUMN verdict_disposition text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts
    DROP COLUMN verdict_evidence_revision,
    DROP COLUMN verdict_attempted_step,
    DROP COLUMN verdict_success,
    DROP COLUMN verdict_outcome,
    DROP COLUMN verdict_reason,
    DROP COLUMN verdict_next_step,
    DROP COLUMN verdict_disposition;
