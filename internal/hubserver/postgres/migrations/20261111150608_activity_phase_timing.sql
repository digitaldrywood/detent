-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE report_event_facts ADD COLUMN phase_timing_status text NOT NULL DEFAULT '';
ALTER TABLE report_event_facts ADD CONSTRAINT report_event_facts_phase_timing_status_check CHECK (phase_timing_status IN ('', 'complete', 'partial')) NOT VALID;
GRANT UPDATE(phase_timing_status) ON report_event_facts TO detent_runtime,detent_platform;

-- +goose Down
SET LOCAL lock_timeout = '5s';
REVOKE UPDATE(phase_timing_status) ON report_event_facts FROM detent_runtime,detent_platform;
ALTER TABLE report_event_facts DROP CONSTRAINT report_event_facts_phase_timing_status_check;
ALTER TABLE report_event_facts DROP COLUMN phase_timing_status;
