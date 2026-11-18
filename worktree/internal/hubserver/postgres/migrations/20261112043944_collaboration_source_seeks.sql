-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout='5s';
DROP INDEX CONCURRENTLY IF EXISTS issues_scheduled_source_pending;
CREATE INDEX CONCURRENTLY issues_scheduled_source_pending ON issues(organization_id,project_id,native_id) WHERE source_revision<>revision;
DROP INDEX CONCURRENTLY IF EXISTS native_comments_scheduled_source_pending;
CREATE INDEX CONCURRENTLY native_comments_scheduled_source_pending ON native_comments(organization_id,project_id,id) WHERE source_revision<>revision;
DROP INDEX CONCURRENTLY IF EXISTS native_comments_scheduled_source_pending_item;
CREATE INDEX CONCURRENTLY native_comments_scheduled_source_pending_item ON native_comments(organization_id,project_id,work_item_id,id) WHERE source_revision<>revision;
DROP INDEX CONCURRENTLY IF EXISTS issues_scheduled_source_records;
CREATE INDEX CONCURRENTLY issues_scheduled_source_records ON issues(organization_id,project_id,source_observed_at DESC,native_id DESC) WHERE source_revision=revision AND source_scheduled;
DROP INDEX CONCURRENTLY IF EXISTS native_comments_scheduled_source_records;
CREATE INDEX CONCURRENTLY native_comments_scheduled_source_records ON native_comments(organization_id,project_id,source_observed_at DESC,id DESC) WHERE source_revision=revision AND source_scheduled;
DROP INDEX CONCURRENTLY IF EXISTS native_comments_scheduled_source_records_item;
CREATE INDEX CONCURRENTLY native_comments_scheduled_source_records_item ON native_comments(organization_id,project_id,work_item_id,source_observed_at DESC,id DESC) WHERE source_revision=revision AND source_scheduled;
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'scheduled source provenance cannot be discarded'; END; $$;
-- +goose StatementEnd
