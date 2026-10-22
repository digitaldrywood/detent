-- +goose Up
ALTER TABLE native_attempts ADD COLUMN detail_attachment_id TEXT REFERENCES attachments(id);
ALTER TABLE native_attempts ADD COLUMN policy_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN machine_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN runner_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN effort TEXT NOT NULL DEFAULT '';

ALTER TABLE native_attempts ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN cached_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN cost_estimate REAL NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN final_outcome TEXT NOT NULL DEFAULT '';

ALTER TABLE native_attempts ADD COLUMN checkpoint_attachment_id TEXT REFERENCES attachments(id);
ALTER TABLE native_attempts ADD COLUMN execution_role TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN runtime_phase TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN local_attempt_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN runtime_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN heartbeat_at TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN failure_class TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN failure_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN finalization_settled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN finalization_changed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN finalization_change_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN finalization_version_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE native_attempts DROP COLUMN finalization_version_id;
ALTER TABLE native_attempts DROP COLUMN finalization_change_id;
ALTER TABLE native_attempts DROP COLUMN finalization_changed;
ALTER TABLE native_attempts DROP COLUMN finalization_settled;
ALTER TABLE native_attempts DROP COLUMN failure_stage;
ALTER TABLE native_attempts DROP COLUMN failure_class;
ALTER TABLE native_attempts DROP COLUMN heartbeat_at;
ALTER TABLE native_attempts DROP COLUMN runtime_generation;
ALTER TABLE native_attempts DROP COLUMN local_attempt_id;
ALTER TABLE native_attempts DROP COLUMN runtime_phase;
ALTER TABLE native_attempts DROP COLUMN execution_role;
ALTER TABLE native_attempts DROP COLUMN checkpoint_attachment_id;
ALTER TABLE native_attempts DROP COLUMN final_outcome;
ALTER TABLE native_attempts DROP COLUMN cost_estimate;
ALTER TABLE native_attempts DROP COLUMN output_tokens;
ALTER TABLE native_attempts DROP COLUMN cached_input_tokens;
ALTER TABLE native_attempts DROP COLUMN input_tokens;
ALTER TABLE native_attempts DROP COLUMN effort;
ALTER TABLE native_attempts DROP COLUMN model;
ALTER TABLE native_attempts DROP COLUMN provider;
ALTER TABLE native_attempts DROP COLUMN runner_id;
ALTER TABLE native_attempts DROP COLUMN session_id;
ALTER TABLE native_attempts DROP COLUMN machine_id;
ALTER TABLE native_attempts DROP COLUMN policy_id;
ALTER TABLE native_attempts DROP COLUMN detail_attachment_id;
