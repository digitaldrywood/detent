-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh ADD COLUMN observed_request_count bigint;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_observed_request_count CHECK (observed_request_count>0 AND observed_request_count<=request_count) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh DROP COLUMN observed_request_count;
