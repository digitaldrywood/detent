-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE native_attempts
 ADD COLUMN IF NOT EXISTS identity_sequence bigint,
 ADD COLUMN IF NOT EXISTS identity_execution_role text,
 ADD COLUMN IF NOT EXISTS identity_execution_backend text,
 ADD COLUMN IF NOT EXISTS identity_execution_model text,
 ADD COLUMN IF NOT EXISTS identity_execution_reasoning_effort text,
 ADD COLUMN IF NOT EXISTS identity_selection_effort_clamped_from text,
 ADD COLUMN IF NOT EXISTS identity_selection_policy text,
 ADD COLUMN IF NOT EXISTS identity_selection_policy_source text,
 ADD COLUMN IF NOT EXISTS identity_selection_reason text,
 ADD COLUMN IF NOT EXISTS identity_selection_level text,
 ADD COLUMN IF NOT EXISTS identity_selection_requested_model text,
 ADD COLUMN IF NOT EXISTS identity_selection_model_source text,
 ADD COLUMN IF NOT EXISTS identity_selection_effort_source text,
 ADD COLUMN IF NOT EXISTS identity_selection_fallback_reason text,
 ADD COLUMN IF NOT EXISTS identity_selection_backend_source text,
 ADD COLUMN IF NOT EXISTS identity_selection_route_source text,
 ADD COLUMN IF NOT EXISTS identity_backend_id text,
 ADD COLUMN IF NOT EXISTS identity_backend_kind text,
 ADD COLUMN IF NOT EXISTS identity_route text,
 ADD COLUMN IF NOT EXISTS identity_role text,
 ADD COLUMN IF NOT EXISTS identity_provider text,
 ADD COLUMN IF NOT EXISTS identity_provider_provenance text,
 ADD COLUMN IF NOT EXISTS identity_requested_model text,
 ADD COLUMN IF NOT EXISTS identity_requested_model_provenance text,
 ADD COLUMN IF NOT EXISTS identity_resolved_model text,
 ADD COLUMN IF NOT EXISTS identity_resolved_model_provenance text,
 ADD COLUMN IF NOT EXISTS identity_reasoning_effort text,
 ADD COLUMN IF NOT EXISTS identity_reasoning_effort_provenance text,
 ADD COLUMN IF NOT EXISTS identity_service_tier text,
 ADD COLUMN IF NOT EXISTS identity_service_tier_provenance text,
 ADD COLUMN IF NOT EXISTS identity_observed_at timestamptz;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='native_attempts'::regclass AND conname='native_attempt_identity_valid') THEN
  ALTER TABLE native_attempts ADD CONSTRAINT native_attempt_identity_valid CHECK(
   (identity_sequence IS NULL OR identity_sequence BETWEEN 1 AND sequence)
   AND ((identity_execution_role IS NULL AND identity_execution_backend IS NULL AND identity_execution_model IS NULL AND identity_execution_reasoning_effort IS NULL)
    OR (identity_execution_role IS NOT NULL AND identity_execution_backend IS NOT NULL AND identity_execution_model IS NOT NULL))
   AND (identity_provider IS NULL OR identity_provider_provenance IS NOT NULL)
   AND (identity_requested_model IS NULL OR identity_requested_model_provenance IS NOT NULL)
   AND (identity_resolved_model IS NULL OR identity_resolved_model_provenance IS NOT NULL)
   AND (identity_reasoning_effort IS NULL OR identity_reasoning_effort_provenance IS NOT NULL)
   AND (identity_service_tier IS NULL OR identity_service_tier_provenance IS NOT NULL)
   AND (identity_provider_provenance IS NULL OR identity_provider_provenance IN ('unknown','configured','runtime','catalog_default'))
   AND (identity_requested_model_provenance IS NULL OR identity_requested_model_provenance IN ('unknown','configured','runtime','catalog_default'))
   AND (identity_resolved_model_provenance IS NULL OR identity_resolved_model_provenance IN ('unknown','configured','runtime','catalog_default'))
   AND (identity_reasoning_effort_provenance IS NULL OR identity_reasoning_effort_provenance IN ('unknown','configured','runtime','catalog_default'))
   AND (identity_service_tier_provenance IS NULL OR identity_service_tier_provenance IN ('unknown','configured','runtime','catalog_default'))
  ) NOT VALID;
 END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE native_attempts DROP CONSTRAINT IF EXISTS native_attempt_identity_valid;
ALTER TABLE native_attempts
 DROP COLUMN IF EXISTS identity_observed_at,
 DROP COLUMN IF EXISTS identity_service_tier_provenance,
 DROP COLUMN IF EXISTS identity_service_tier,
 DROP COLUMN IF EXISTS identity_reasoning_effort_provenance,
 DROP COLUMN IF EXISTS identity_reasoning_effort,
 DROP COLUMN IF EXISTS identity_resolved_model_provenance,
 DROP COLUMN IF EXISTS identity_resolved_model,
 DROP COLUMN IF EXISTS identity_requested_model_provenance,
 DROP COLUMN IF EXISTS identity_requested_model,
 DROP COLUMN IF EXISTS identity_provider_provenance,
 DROP COLUMN IF EXISTS identity_provider,
 DROP COLUMN IF EXISTS identity_role,
 DROP COLUMN IF EXISTS identity_route,
 DROP COLUMN IF EXISTS identity_backend_kind,
 DROP COLUMN IF EXISTS identity_backend_id,
 DROP COLUMN IF EXISTS identity_selection_route_source,
 DROP COLUMN IF EXISTS identity_selection_backend_source,
 DROP COLUMN IF EXISTS identity_selection_fallback_reason,
 DROP COLUMN IF EXISTS identity_selection_effort_source,
 DROP COLUMN IF EXISTS identity_selection_model_source,
 DROP COLUMN IF EXISTS identity_selection_requested_model,
 DROP COLUMN IF EXISTS identity_selection_level,
 DROP COLUMN IF EXISTS identity_selection_reason,
 DROP COLUMN IF EXISTS identity_selection_policy_source,
 DROP COLUMN IF EXISTS identity_selection_policy,
 DROP COLUMN IF EXISTS identity_selection_effort_clamped_from,
 DROP COLUMN IF EXISTS identity_execution_reasoning_effort,
 DROP COLUMN IF EXISTS identity_execution_model,
 DROP COLUMN IF EXISTS identity_execution_backend,
 DROP COLUMN IF EXISTS identity_execution_role,
 DROP COLUMN IF EXISTS identity_sequence;
RESET lock_timeout;
