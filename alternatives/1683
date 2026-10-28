-- +goose Up
ALTER TABLE machines ADD COLUMN sprite_name TEXT;
ALTER TABLE machines ADD COLUMN sprite_woken_at TEXT;
ALTER TABLE machines ADD COLUMN sprite_wake_failed_at TEXT;
ALTER TABLE machines ADD COLUMN machine_facts_ready INTEGER;
ALTER TABLE machines ADD COLUMN admission_facts_ready INTEGER;
CREATE INDEX machines_sprite_name ON machines(organization_id,sprite_name,hostname);
CREATE INDEX machines_facts_unknown ON machines(id) WHERE machine_facts_ready IS NULL OR admission_facts_ready IS NULL;
ALTER TABLE runner_identities ADD COLUMN routing_facts_ready INTEGER;
ALTER TABLE runner_identities ADD COLUMN local_check_facts_ready INTEGER;
CREATE INDEX runner_routing_facts_unknown ON runner_identities(id) WHERE routing_facts_ready IS NULL OR local_check_facts_ready IS NULL;
CREATE TABLE runner_project_ranks (
  runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  rank INTEGER NOT NULL,
  PRIMARY KEY(runner_id,project_id)
);
ALTER TABLE native_commands ADD COLUMN response_pending INTEGER;
ALTER TABLE native_commands ADD COLUMN response_request_id TEXT;
CREATE INDEX native_commands_request ON native_commands(organization_id,operation,response_request_id);
CREATE INDEX native_commands_completed ON native_commands(organization_id,operation,created_at DESC) WHERE response_pending=0 OR response_pending IS NULL;
CREATE INDEX native_commands_unknown_request ON native_commands(organization_id,operation) WHERE response_pending IS NULL;
CREATE INDEX native_commands_facts_unknown ON native_commands(response_pending) WHERE response_pending IS NULL;
ALTER TABLE native_attempts ADD COLUMN usage_facts_ready INTEGER;
ALTER TABLE native_attempts ADD COLUMN process_peak_memory_bytes INTEGER;
ALTER TABLE native_attempts ADD COLUMN process_user_cpu_seconds REAL;
ALTER TABLE native_attempts ADD COLUMN process_system_cpu_seconds REAL;
ALTER TABLE native_attempts ADD COLUMN process_wall_seconds REAL;
CREATE INDEX native_attempts_usage_unknown ON native_attempts(id) WHERE usage_facts_ready IS NULL;
CREATE INDEX native_attempts_recommendation ON native_attempts(organization_id,lease_id,status,started_at DESC) WHERE process_peak_memory_bytes>0 OR usage_facts_ready IS NULL;

CREATE TABLE runner_local_checks (
 runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL,
 commit_name TEXT NOT NULL,
 commit_email TEXT NOT NULL,
 commit_signing TEXT NOT NULL,
 checkout_message TEXT NOT NULL,
 checkout_fix TEXT NOT NULL,
 runner_setup_declared INTEGER,
 setup TEXT NOT NULL,
 checkout TEXT NOT NULL,
 configuration TEXT NOT NULL,
 checkout_at TEXT NOT NULL,
 configuration_at TEXT NOT NULL,
 doctor_at TEXT NOT NULL,
 provider_at TEXT NOT NULL,
 doctor TEXT NOT NULL,
 provider TEXT NOT NULL,
 observed_at TEXT NOT NULL,
 PRIMARY KEY(runner_id,project_id)
);
CREATE TABLE runner_local_check_providers (runner_id TEXT NOT NULL,project_id TEXT NOT NULL,position INTEGER NOT NULL,kind TEXT NOT NULL,PRIMARY KEY(runner_id,project_id,position),FOREIGN KEY(runner_id,project_id) REFERENCES runner_local_checks(runner_id,project_id) ON DELETE CASCADE);
CREATE TABLE runner_admission_observations (
 runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL,
 runner_revision INTEGER NOT NULL,
 received_at TEXT NOT NULL,
 observed_at TEXT NOT NULL,
 policy_id TEXT NOT NULL,
 last_refresh_at TEXT NOT NULL,
 refresh_duration INTEGER NOT NULL,
 PRIMARY KEY(runner_id,project_id)
);
CREATE TABLE runner_admission_selectors (
 runner_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 kind TEXT NOT NULL,
 position INTEGER NOT NULL,
 value TEXT NOT NULL,
 PRIMARY KEY(runner_id,project_id,kind,position),
 FOREIGN KEY(runner_id,project_id) REFERENCES runner_admission_observations(runner_id,project_id) ON DELETE CASCADE
);

CREATE TABLE runner_configuration_requests (runner_id TEXT PRIMARY KEY REFERENCES runner_identities(id) ON DELETE CASCADE,runner_revision INTEGER NOT NULL,allow_local_binding INTEGER,request_id TEXT NOT NULL,operation TEXT NOT NULL,project_id TEXT NOT NULL,expected_config_revision TEXT NOT NULL,expected_policy_id TEXT NOT NULL,policy_id TEXT NOT NULL,source_revision TEXT NOT NULL,checkpoint TEXT NOT NULL);

-- +goose Down
DROP TABLE runner_configuration_requests;

DROP TABLE runner_admission_selectors;
DROP TABLE runner_admission_observations;
DROP TABLE runner_local_check_providers;
DROP TABLE runner_local_checks;

DROP INDEX native_attempts_recommendation;
DROP INDEX native_attempts_usage_unknown;
ALTER TABLE native_attempts DROP COLUMN process_wall_seconds;
ALTER TABLE native_attempts DROP COLUMN process_system_cpu_seconds;
ALTER TABLE native_attempts DROP COLUMN process_user_cpu_seconds;
ALTER TABLE native_attempts DROP COLUMN process_peak_memory_bytes;
ALTER TABLE native_attempts DROP COLUMN usage_facts_ready;
DROP INDEX native_commands_facts_unknown;
DROP INDEX native_commands_unknown_request;
DROP INDEX native_commands_completed;
DROP INDEX native_commands_request;
ALTER TABLE native_commands DROP COLUMN response_request_id;
ALTER TABLE native_commands DROP COLUMN response_pending;
DROP TABLE runner_project_ranks;
DROP INDEX runner_routing_facts_unknown;
ALTER TABLE runner_identities DROP COLUMN local_check_facts_ready;
ALTER TABLE runner_identities DROP COLUMN routing_facts_ready;
DROP INDEX machines_facts_unknown;
DROP INDEX machines_sprite_name;
ALTER TABLE machines DROP COLUMN admission_facts_ready;
ALTER TABLE machines DROP COLUMN machine_facts_ready;
ALTER TABLE machines DROP COLUMN sprite_wake_failed_at;
ALTER TABLE machines DROP COLUMN sprite_woken_at;
ALTER TABLE machines DROP COLUMN sprite_name;
