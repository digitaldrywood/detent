-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_observation_shape;
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_source_shape;
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_merge_shape;
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_webhook_source;

-- +goose Down
SELECT 1;
