-- +goose Up
CREATE TABLE project_runner_setup (
    runner_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    PRIMARY KEY (runner_id, project_id)
);

-- +goose Down
DROP TABLE project_runner_setup;
