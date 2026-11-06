-- +goose Up
CREATE TABLE native_pr_publications (
    version_id TEXT PRIMARY KEY REFERENCES change_versions(id),
    change_id TEXT NOT NULL REFERENCES change_requests(id),
    repository TEXT NOT NULL,
    base_ref TEXT NOT NULL,
    branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    source_attempt_id TEXT NOT NULL,
    recorder_attempt_id TEXT NOT NULL REFERENCES native_attempts(id),
    provider TEXT NOT NULL,
    external_id TEXT NOT NULL,
    external_url TEXT NOT NULL
);

-- +goose Down
DROP TABLE native_pr_publications;
