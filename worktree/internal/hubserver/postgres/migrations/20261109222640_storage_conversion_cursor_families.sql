-- +goose Up
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check
 CHECK (family IN ('runs','versions','evidence','search','core-work-events'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'storage conversion cursor family repair is forward-only; preserve established cursor families';
END $$;
-- +goose StatementEnd
