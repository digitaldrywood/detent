-- +goose Up
ALTER TABLE native_attempts ADD COLUMN health_projected INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN health_failure_text TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN failure_attribution TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN landing_landed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN landing_refusal_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN landing_change_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN landing_version_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN landing_merge_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN landing_observed_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE native_attempts DROP COLUMN landing_observed_at;
ALTER TABLE native_attempts DROP COLUMN landing_merge_sha;
ALTER TABLE native_attempts DROP COLUMN landing_version_id;
ALTER TABLE native_attempts DROP COLUMN landing_change_id;
ALTER TABLE native_attempts DROP COLUMN landing_refusal_kind;
ALTER TABLE native_attempts DROP COLUMN landing_landed;
ALTER TABLE native_attempts DROP COLUMN failure_attribution;
ALTER TABLE native_attempts DROP COLUMN health_failure_text;
ALTER TABLE native_attempts DROP COLUMN health_projected;
