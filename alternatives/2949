-- +goose Up
CREATE INDEX runner_provider_reports_shared_account_idx
    ON runner_provider_reports (organization_id, provider, shared_account_alias, runner_id, backend)
    WHERE shared_account_alias <> '';

-- +goose Down
DROP INDEX runner_provider_reports_shared_account_idx;
