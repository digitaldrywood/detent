-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';

DROP INDEX CONCURRENTLY IF EXISTS change_recovery_discussion_idx;
CREATE INDEX CONCURRENTLY change_recovery_discussion_idx
ON change_evidence (organization_id,change_id,version_id,sequence DESC)
WHERE kind='discussion';

DROP INDEX CONCURRENTLY IF EXISTS change_recovery_landing_review_idx;
CREATE INDEX CONCURRENTLY change_recovery_landing_review_idx
ON change_evidence (organization_id,change_id,version_id,sequence DESC)
WHERE kind='review' AND review_kind='security_audit' AND review_landing;

DROP INDEX CONCURRENTLY IF EXISTS change_recovery_unknown_approval_idx;
CREATE INDEX CONCURRENTLY change_recovery_unknown_approval_idx
ON change_evidence (organization_id,change_id,sequence DESC)
WHERE kind='review' AND review_kind='' AND decision='approved' AND version_id IS NOT NULL;

RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS change_recovery_unknown_approval_idx;
DROP INDEX CONCURRENTLY IF EXISTS change_recovery_landing_review_idx;
DROP INDEX CONCURRENTLY IF EXISTS change_recovery_discussion_idx;
