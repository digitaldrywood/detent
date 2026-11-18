-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check CHECK (family IN ('runs','versions','evidence','search','core-work-events','issue-archives','issue-archive-retirement','issue-retention')) NOT VALID;
ALTER TABLE storage_conversion_cursors VALIDATE CONSTRAINT storage_conversion_cursors_family_check;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM storage_conversion_cursors WHERE family='issue-retention') THEN
        RAISE EXCEPTION 'issue retention progress requires an explicit export before rollback';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check CHECK (family IN ('runs','versions','evidence','search','core-work-events','issue-archives','issue-archive-retirement')) NOT VALID;
ALTER TABLE storage_conversion_cursors VALIDATE CONSTRAINT storage_conversion_cursors_family_check;
