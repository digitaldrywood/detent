-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE machines ADD COLUMN sprite_woken_at timestamptz;
ALTER TABLE machines ADD COLUMN sprite_wake_failed_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM machines WHERE sprite_woken_at IS NOT NULL OR sprite_wake_failed_at IS NOT NULL) THEN
  RAISE EXCEPTION 'machine Sprite wake facts prevent rollback';
 END IF;
END; $$;
-- +goose StatementEnd
ALTER TABLE machines DROP COLUMN sprite_wake_failed_at;
ALTER TABLE machines DROP COLUMN sprite_woken_at;
