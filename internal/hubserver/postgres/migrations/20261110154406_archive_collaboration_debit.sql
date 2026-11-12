-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE archive_objects ADD COLUMN collaboration_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE archive_objects ADD CONSTRAINT archive_objects_collaboration_nonnegative CHECK (collaboration_bytes >= 0) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM archive_objects WHERE collaboration_bytes <> 0) THEN
  RAISE EXCEPTION 'archive collaboration debits require reconciliation before rollback';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE archive_objects DROP COLUMN collaboration_bytes;
