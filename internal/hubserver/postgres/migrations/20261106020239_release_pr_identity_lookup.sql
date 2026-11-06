-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY change_versions_release_pr_idx ON change_versions (organization_id, repository, external_id, change_id) WHERE external_provider = 'github';

CREATE INDEX CONCURRENTLY change_evidence_release_review_idx ON change_evidence (organization_id, change_id, version_id, sequence DESC) WHERE kind = 'review';

-- +goose Down
DROP INDEX CONCURRENTLY change_evidence_release_review_idx;
DROP INDEX CONCURRENTLY change_versions_release_pr_idx;
