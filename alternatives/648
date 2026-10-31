-- +goose Up
CREATE TABLE github_user_connections (
    owner_subject TEXT PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    access_token TEXT NOT NULL,
    expires_at TEXT,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);

-- +goose Down
DROP TABLE github_user_connections;
