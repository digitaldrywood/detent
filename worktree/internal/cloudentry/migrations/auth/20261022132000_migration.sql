-- +goose Up
CREATE TABLE oauth_client_redirect_uris (
    client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    uri TEXT NOT NULL,
    PRIMARY KEY (client_id, position)
);

-- +goose Down
DROP TABLE oauth_client_redirect_uris;
