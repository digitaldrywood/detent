-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE attempt_usage VALIDATE CONSTRAINT attempt_usage_cumulative_valid;
ALTER TABLE attempt_usage VALIDATE CONSTRAINT attempt_usage_sequence_valid;
ALTER TABLE attempt_usage VALIDATE CONSTRAINT attempt_usage_origin_valid;
ALTER TABLE report_usage_current VALIDATE CONSTRAINT report_usage_kind_valid;
ALTER TABLE native_attempts VALIDATE CONSTRAINT native_attempt_usage_projection_valid;
RESET lock_timeout;

-- +goose Down
SELECT 1;
