-- +goose Up
CREATE TABLE usage_cost_observations (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  provider_account TEXT NOT NULL,
  resource_id TEXT NOT NULL,
  bucket TEXT NOT NULL,
  metric TEXT NOT NULL,
  source_id TEXT NOT NULL,
  period_start TEXT NOT NULL,
  period_end TEXT NOT NULL CHECK (period_end > period_start),
  revision INTEGER NOT NULL CHECK (revision > 0),
  observation_json TEXT NOT NULL CHECK (json_valid(observation_json)),
  received_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, provider, provider_account, source_id, period_start, period_end, revision)
);
CREATE INDEX usage_cost_window_idx ON usage_cost_observations(organization_id, period_end, period_start, project_id);
CREATE INDEX usage_cost_resource_idx ON usage_cost_observations(organization_id, provider, provider_account, resource_id, bucket, metric);

-- +goose Down
DROP TABLE usage_cost_observations;
