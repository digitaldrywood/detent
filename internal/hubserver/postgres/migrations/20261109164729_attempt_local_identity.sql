-- +goose Up
ALTER TABLE native_attempts
    ADD COLUMN local_attempt_id bigint CHECK (local_attempt_id > 0),
    ADD COLUMN local_attempt_id_recorded boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT native_attempts_local_identity_recorded CHECK (local_attempt_id IS NULL OR local_attempt_id_recorded);
CREATE INDEX native_attempts_local_identity_idx
    ON native_attempts (organization_id, issue_id, local_attempt_id, id)
    WHERE local_attempt_id IS NOT NULL;

CREATE INDEX native_attempts_local_identity_unknown_idx
    ON native_attempts (organization_id, issue_id)
    WHERE NOT local_attempt_id_recorded;

-- +goose Down
DROP INDEX native_attempts_local_identity_unknown_idx;
DROP INDEX native_attempts_local_identity_idx;
ALTER TABLE native_attempts
    DROP CONSTRAINT native_attempts_local_identity_recorded,
    DROP COLUMN local_attempt_id_recorded,
    DROP COLUMN local_attempt_id;
