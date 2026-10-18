-- +goose Up
ALTER TABLE projects ADD COLUMN archive_completed_after_days INTEGER DEFAULT 30 CHECK (archive_completed_after_days IS NULL OR archive_completed_after_days IN (7, 14, 30, 60, 90));
ALTER TABLE projects ADD COLUMN archive_cancelled_after_days INTEGER DEFAULT 7 CHECK (archive_cancelled_after_days IS NULL OR archive_cancelled_after_days IN (7, 14, 30, 60, 90));

-- +goose Down
ALTER TABLE projects DROP COLUMN archive_cancelled_after_days;
ALTER TABLE projects DROP COLUMN archive_completed_after_days;
