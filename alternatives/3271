-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';

DROP INDEX CONCURRENTLY IF EXISTS change_prior_approval_version_idx;
CREATE INDEX CONCURRENTLY change_prior_approval_version_idx
ON change_evidence (organization_id,change_id,version_id,sequence)
WHERE kind='review' AND decision='approved' AND review_kind IN ('human','');
DROP INDEX CONCURRENTLY IF EXISTS change_prior_approval_idx;

RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';

DROP INDEX CONCURRENTLY IF EXISTS change_prior_approval_idx;
CREATE INDEX CONCURRENTLY change_prior_approval_idx
ON change_evidence (organization_id,change_id,sequence DESC)
WHERE kind='review' AND decision='approved' AND review_kind IN ('human','');
DROP INDEX CONCURRENTLY IF EXISTS change_prior_approval_version_idx;

RESET lock_timeout;
