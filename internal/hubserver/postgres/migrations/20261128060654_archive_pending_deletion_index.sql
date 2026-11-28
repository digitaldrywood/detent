-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION immutable_attachment_archive() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 stable attachments%ROWTYPE;
BEGIN
 IF OLD.kind<>'archive' THEN RETURN NEW; END IF;
 stable := NEW;
 stable.deleted_at := OLD.deleted_at;
 stable.object_deleted_at := OLD.object_deleted_at;
 IF stable IS DISTINCT FROM OLD
 OR OLD.object_key !~ '^orgs/[^/]+/blobs/sha256/[0-9a-f]{2}/[0-9a-f]{64}/[0-9a-f]{64}$'
 OR NOT (
  (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL AND NEW.object_deleted_at IS NULL)
  OR (OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at
      AND OLD.object_deleted_at IS NULL AND NEW.object_deleted_at IS NOT NULL)
 ) THEN
  RAISE EXCEPTION 'immutable archive' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS attachments_archive_pending_deletion ON attachments (organization_id,deleted_at,id)
WHERE kind='archive' AND pending_content IS NULL AND deleted_at IS NOT NULL AND object_deleted_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS attachments_archive_pending_deletion;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION immutable_attachment_archive() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.kind='archive' THEN RAISE EXCEPTION 'immutable archive' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
