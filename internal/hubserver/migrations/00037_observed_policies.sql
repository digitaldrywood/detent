-- +goose Up
-- The last repository policy a runner resolved for a project and could not run
-- because it differs from the approved policy, or because none is approved yet.
-- Project settings offers it for approval, so an owner approves what the runner
-- actually resolved instead of pasting the descriptor by hand.
CREATE TABLE project_observed_policies (
  scope TEXT PRIMARY KEY,
  policy_id TEXT NOT NULL,
  descriptor_json TEXT NOT NULL CHECK (json_valid(descriptor_json)),
  runner_id TEXT NOT NULL,
  observed_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE project_observed_policies;
