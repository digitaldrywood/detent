-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issue_labels_claim_value_lookup;
CREATE INDEX CONCURRENTLY issue_labels_claim_value_lookup ON issue_labels (organization_id, project_id, lower(btrim(label)), work_item_id);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issue_labels_claim_value_lookup;
RESET lock_timeout;
