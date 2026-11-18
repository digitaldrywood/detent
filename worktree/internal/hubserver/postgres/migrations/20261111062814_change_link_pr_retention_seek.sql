-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_issue_links_retained_pr_idx;
CREATE INDEX CONCURRENTLY change_issue_links_retained_pr_idx ON change_issue_links (organization_id,project_id,work_item_id,created_at DESC,change_id DESC) WHERE retained_pr_version_id IS NOT NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_issue_links_retained_pr_idx;
RESET lock_timeout;
