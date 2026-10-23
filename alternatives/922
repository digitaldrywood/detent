-- +goose Up
CREATE TABLE oauth_spent_refresh_tokens (
    token_hash TEXT PRIMARY KEY,
    grant_id TEXT NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);
CREATE INDEX oauth_spent_refresh_tokens_grant ON oauth_spent_refresh_tokens(grant_id);

-- +goose Down
DROP TABLE oauth_spent_refresh_tokens;
