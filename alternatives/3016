-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS attachments_pending_deletion ON attachments(organization_id,created_at,id)
WHERE kind='attachment' AND deleted_at IS NOT NULL AND object_deleted_at IS NULL
AND NOT customer_owned AND storage_binding_id='' AND pending_content IS NULL
AND object_key IN ('','orgs/' || organization_id || '/attachments/' || id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS attachments_orphan_expiry ON attachments(organization_id,created_at,id)
WHERE kind='attachment' AND deleted_at IS NULL AND object_deleted_at IS NULL
AND retention_class IN ('','expiring') AND work_item_id IS NULL
AND NOT customer_owned AND storage_binding_id='' AND pending_content IS NULL
AND object_key IN ('','orgs/' || organization_id || '/attachments/' || id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS attachments_orphan_expiry;
DROP INDEX CONCURRENTLY IF EXISTS attachments_pending_deletion;
