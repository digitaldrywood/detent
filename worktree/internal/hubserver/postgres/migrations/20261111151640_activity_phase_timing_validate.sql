-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE report_event_facts VALIDATE CONSTRAINT report_event_facts_phase_timing_status_check;
RESET lock_timeout;

-- +goose Down
SELECT 1;
