-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout='5s';
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_issue_match;
CREATE INDEX CONCURRENTLY landing_receipt_issue_match ON change_landing_receipts(organization_id,issue_id,repository_identity,merge_sha,fencing_token DESC,attempt_id DESC);
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_issue_recent;
CREATE INDEX CONCURRENTLY landing_receipt_issue_recent ON change_landing_receipts(organization_id,issue_id,repository_identity,fencing_token DESC,attempt_id DESC);
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_change_match;
CREATE INDEX CONCURRENTLY landing_receipt_change_match ON change_landing_receipts(organization_id,issue_id,change_id,repository_identity,merge_sha,fencing_token DESC,attempt_id DESC);
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_change_recent;
CREATE INDEX CONCURRENTLY landing_receipt_change_recent ON change_landing_receipts(organization_id,issue_id,change_id,repository_identity,fencing_token DESC,attempt_id DESC);
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_project_match;
CREATE INDEX CONCURRENTLY landing_receipt_project_match ON change_landing_receipts(organization_id,project_id,repository_identity,merge_sha,fencing_token DESC,attempt_id DESC);
DROP INDEX CONCURRENTLY IF EXISTS landing_receipt_project_recent;
CREATE INDEX CONCURRENTLY landing_receipt_project_recent ON change_landing_receipts(organization_id,project_id,repository_identity,fencing_token DESC,attempt_id DESC);
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'landing receipt identity cannot be discarded'; END; $$;
-- +goose StatementEnd
