-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts ADD COLUMN failure_sequence bigint;
ALTER TABLE native_attempts ADD COLUMN failure_example text;
ALTER TABLE native_attempts ADD COLUMN failure_class text;
ALTER TABLE native_attempts ADD COLUMN failure_landing_observed_at timestamptz;
ALTER TABLE native_attempts ADD CONSTRAINT native_attempt_failure_summary CHECK (
    (failure_sequence IS NULL OR failure_sequence BETWEEN 1 AND sequence)
    AND (failure_example IS NULL) = (failure_class IS NULL)
    AND (failure_example IS NULL OR failure_sequence IS NOT NULL AND octet_length(failure_example) BETWEEN 1 AND 4096)
    AND (failure_class IS NULL OR failure_class IN ('attempt', 'merge', 'infrastructure'))
    AND (failure_landing_observed_at IS NULL OR failure_example IS NOT NULL)
) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts DROP CONSTRAINT native_attempt_failure_summary;
ALTER TABLE native_attempts DROP COLUMN failure_landing_observed_at;
ALTER TABLE native_attempts DROP COLUMN failure_class;
ALTER TABLE native_attempts DROP COLUMN failure_example;
ALTER TABLE native_attempts DROP COLUMN failure_sequence;
