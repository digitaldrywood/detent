-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_problem_reports ADD COLUMN cached_capabilities boolean NOT NULL DEFAULT false;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_problem_reports DROP COLUMN cached_capabilities;
