-- +goose Up
ALTER TABLE report_event_facts
 ADD COLUMN process_evidence_status text NOT NULL DEFAULT 'unknown' CHECK(process_evidence_status IN ('unknown','unavailable','usable')),
 ADD COLUMN process_peak_memory_bytes bigint NOT NULL DEFAULT 0 CHECK(process_peak_memory_bytes>=0),
 ADD COLUMN process_user_cpu_seconds double precision NOT NULL DEFAULT 0 CHECK(process_user_cpu_seconds>=0 AND process_user_cpu_seconds<'Infinity'::double precision),
 ADD COLUMN process_system_cpu_seconds double precision NOT NULL DEFAULT 0 CHECK(process_system_cpu_seconds>=0 AND process_system_cpu_seconds<'Infinity'::double precision),
 ADD COLUMN process_wall_seconds double precision NOT NULL DEFAULT 0 CHECK(process_wall_seconds>=0 AND process_wall_seconds<'Infinity'::double precision),
 ADD COLUMN process_lease_id text NOT NULL DEFAULT '',
 ADD COLUMN process_fencing_token bigint NOT NULL DEFAULT 0 CHECK(process_fencing_token>=0),
 ADD COLUMN process_runner_id text NOT NULL DEFAULT '',
 ADD COLUMN process_machine_id text NOT NULL DEFAULT '',
 ADD CONSTRAINT report_process_usage_present CHECK(process_evidence_status<>'usable' OR (process_peak_memory_bytes>0 AND process_wall_seconds>0));
GRANT UPDATE(process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,process_lease_id,process_fencing_token,process_runner_id,process_machine_id) ON report_event_facts TO detent_runtime,detent_platform;

-- +goose Down
ALTER TABLE report_event_facts DROP CONSTRAINT report_process_usage_present,
 DROP COLUMN process_machine_id,
 DROP COLUMN process_runner_id,
 DROP COLUMN process_fencing_token,
 DROP COLUMN process_lease_id,
 DROP COLUMN process_wall_seconds,
 DROP COLUMN process_system_cpu_seconds,
 DROP COLUMN process_user_cpu_seconds,
 DROP COLUMN process_peak_memory_bytes,
 DROP COLUMN process_evidence_status;
