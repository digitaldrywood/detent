-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh VALIDATE CONSTRAINT github_pull_request_refresh_observed_request_count;

-- +goose Down
SELECT 1;
