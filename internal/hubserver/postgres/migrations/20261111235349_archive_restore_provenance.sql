-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE issues ADD COLUMN archive_restore_known boolean NOT NULL DEFAULT false;
ALTER TABLE issues ALTER COLUMN archive_restore_known SET DEFAULT true;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM issues WHERE NOT archive_restore_known) THEN
        RAISE EXCEPTION 'unknown restore provenance prevents rollback';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE issues DROP COLUMN archive_restore_known;
