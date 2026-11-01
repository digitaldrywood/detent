-- +goose Up
CREATE INDEX change_requests_claim_version_idx ON change_requests(organization_id,project_id,claim_version_id);

-- +goose Down
DROP INDEX change_requests_claim_version_idx;
