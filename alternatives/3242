-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh ADD COLUMN merge_policy_bound boolean NOT NULL DEFAULT false, ADD COLUMN repository_default_branch text;
UPDATE github_pull_request_refresh SET merge_policy_bound=true WHERE source_kind='webhook' AND merged_at IS NOT NULL;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_policy_binding CHECK (NOT merge_policy_bound OR merged_at IS NOT NULL) NOT VALID;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_repository_default CHECK (repository_default_branch IS NULL OR repository_default_branch<>'') NOT VALID;
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_policy_binding;
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_repository_default;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'verified pull request source authority cannot be discarded'; END; $$;
-- +goose StatementEnd
