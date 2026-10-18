-- +goose Up
CREATE TABLE runner_provider_usage_windows (
    runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
    backend TEXT NOT NULL,
    kind TEXT NOT NULL,
    percent_used REAL CHECK (percent_used >= 0 AND percent_used <= 100),
    reset_at TEXT,
    observed_at TEXT,
    PRIMARY KEY (runner_id, backend, kind)
);

-- +goose Down
DROP TABLE runner_provider_usage_windows;
