-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE storage_conversion_cursors ADD COLUMN archive_terminal_at timestamptz;
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check CHECK (family IN ('runs','versions','evidence','search','core-work-events','issue-archives','issue-archive-retirement'));

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM storage_conversion_cursors WHERE family IN ('issue-archives','issue-archive-retirement')) THEN
        RAISE EXCEPTION 'archive cursor recovery data prevents rollback';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check CHECK (family IN ('runs','versions','evidence','search','core-work-events'));
ALTER TABLE storage_conversion_cursors DROP COLUMN archive_terminal_at;
