-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE issues ADD COLUMN archive_restored_at timestamptz;
ALTER TABLE project_settings ALTER COLUMN archive_completed_after_days SET DEFAULT 30, ALTER COLUMN archive_cancelled_after_days SET DEFAULT 7;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM issues WHERE archive_restored_at IS NOT NULL) THEN
        RAISE EXCEPTION 'restore history requires an explicit export before rollback';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE issues DROP COLUMN archive_restored_at;
ALTER TABLE project_settings ALTER COLUMN archive_completed_after_days DROP DEFAULT, ALTER COLUMN archive_cancelled_after_days DROP DEFAULT;
