-- +goose Up
CREATE INDEX pull_requests_repository_url ON pull_requests(repository_id,url);

-- +goose Down
DROP INDEX pull_requests_repository_url;
