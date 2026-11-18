-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=ANY(c.conkey) WHERE c.conrelid='attempt_usage'::regclass AND c.contype='p' AND a.attname='usage_origin') THEN
  ALTER TABLE attempt_usage DROP CONSTRAINT attempt_usage_pkey, ADD CONSTRAINT attempt_usage_pkey PRIMARY KEY USING INDEX attempt_usage_currency_key;
 END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_original_key;
CREATE UNIQUE INDEX CONCURRENTLY attempt_usage_original_key ON attempt_usage(organization_id,attempt_id,period,provider,model);
ALTER TABLE attempt_usage DROP CONSTRAINT attempt_usage_pkey, ADD CONSTRAINT attempt_usage_pkey PRIMARY KEY USING INDEX attempt_usage_original_key;
RESET lock_timeout;
