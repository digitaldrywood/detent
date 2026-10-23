-- +goose Up
CREATE TABLE runner_repository_lists (
    runner_id TEXT PRIMARY KEY REFERENCES runner_identities(id) ON DELETE CASCADE,
    request_id TEXT NOT NULL,
    pending INTEGER NOT NULL CHECK(pending IN (0,1)),
    reason TEXT NOT NULL DEFAULT '',
    listed_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE runner_repositories (
    runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    name TEXT NOT NULL,
    private INTEGER NOT NULL CHECK(private IN (0,1)),
    pushed_at TEXT,
    listed_at TEXT NOT NULL,
    PRIMARY KEY(runner_id,owner,name)
);

-- +goose Down
DROP TABLE runner_repositories;
DROP TABLE runner_repository_lists;
