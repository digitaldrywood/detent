-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE attempt_usage ADD COLUMN IF NOT EXISTS usage_origin text NOT NULL DEFAULT 'import', ADD COLUMN IF NOT EXISTS usage_input_total bigint, ADD COLUMN IF NOT EXISTS usage_cached_input_total bigint, ADD COLUMN IF NOT EXISTS usage_output_total bigint, ADD COLUMN IF NOT EXISTS usage_cost_total double precision, ADD COLUMN IF NOT EXISTS usage_sequence bigint NOT NULL DEFAULT 0;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='attempt_usage'::regclass AND conname='attempt_usage_cumulative_valid') THEN
  ALTER TABLE attempt_usage ADD CONSTRAINT attempt_usage_cumulative_valid CHECK ((usage_input_total IS NULL AND usage_cached_input_total IS NULL AND usage_output_total IS NULL AND usage_cost_total IS NULL) OR (usage_input_total IS NOT NULL AND usage_cached_input_total IS NOT NULL AND usage_output_total IS NOT NULL AND usage_cost_total IS NOT NULL AND usage_input_total>=0 AND usage_cached_input_total>=0 AND usage_output_total>=0 AND usage_cost_total>=0 AND usage_cost_total<'Infinity'::double precision)) NOT VALID;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='attempt_usage'::regclass AND conname='attempt_usage_sequence_valid') THEN
  ALTER TABLE attempt_usage ADD CONSTRAINT attempt_usage_sequence_valid CHECK(usage_sequence>=0) NOT VALID;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='attempt_usage'::regclass AND conname='attempt_usage_origin_valid') THEN
  ALTER TABLE attempt_usage ADD CONSTRAINT attempt_usage_origin_valid CHECK(usage_origin IN ('import','current')) NOT VALID;
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE native_attempts ADD COLUMN IF NOT EXISTS usage_pending_sequence bigint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS usage_projected_sequence bigint NOT NULL DEFAULT 0;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='native_attempts'::regclass AND conname='native_attempt_usage_projection_valid') THEN
  ALTER TABLE native_attempts ADD CONSTRAINT native_attempt_usage_projection_valid CHECK(usage_pending_sequence>=0 AND usage_projected_sequence>=0 AND (usage_pending_sequence=0 OR usage_pending_sequence<=sequence)) NOT VALID;
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE report_usage_current ADD COLUMN IF NOT EXISTS currency text, ADD COLUMN IF NOT EXISTS usage_kind text;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='report_usage_current'::regclass AND conname='report_usage_kind_valid') THEN
  ALTER TABLE report_usage_current ADD CONSTRAINT report_usage_kind_valid CHECK(usage_kind IS NULL OR usage_kind IN ('cumulative','incremental')) NOT VALID;
 END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE native_attempts DROP CONSTRAINT native_attempt_usage_projection_valid;
ALTER TABLE native_attempts DROP COLUMN usage_pending_sequence,DROP COLUMN usage_projected_sequence;
ALTER TABLE report_usage_current DROP CONSTRAINT report_usage_kind_valid;
ALTER TABLE report_usage_current DROP COLUMN usage_kind,DROP COLUMN currency;
ALTER TABLE attempt_usage DROP CONSTRAINT attempt_usage_cumulative_valid,DROP CONSTRAINT attempt_usage_sequence_valid,DROP CONSTRAINT attempt_usage_origin_valid;
ALTER TABLE attempt_usage DROP COLUMN usage_origin,DROP COLUMN usage_input_total,DROP COLUMN usage_cached_input_total,DROP COLUMN usage_output_total,DROP COLUMN usage_cost_total,DROP COLUMN usage_sequence;
RESET lock_timeout;
