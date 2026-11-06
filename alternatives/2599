-- +goose Up
ALTER TABLE policy_revisions ADD COLUMN gate_github_pull_request boolean;

-- +goose Down
ALTER TABLE policy_revisions DROP COLUMN gate_github_pull_request;
