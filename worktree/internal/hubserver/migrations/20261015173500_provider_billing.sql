-- +goose Up
CREATE TABLE runner_provider_billing (
    runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
    backend TEXT NOT NULL,
    billing TEXT NOT NULL CHECK (billing IN ('subscription', 'api_key', 'unknown')),
    PRIMARY KEY (runner_id, backend)
);

-- +goose Down
DROP TABLE runner_provider_billing;
