-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY change_review_summary_tail_idx
ON change_evidence(organization_id,change_id,sequence) WHERE kind='review';

-- +goose Down
DROP INDEX CONCURRENTLY change_review_summary_tail_idx;
