-- +goose Up
-- The last repository policy each runner resolved for a project and could not
-- run because it differs from the approved policy, or because none is approved.
-- Project settings offers it for approval, so an owner approves what the runner
-- actually resolved instead of pasting the descriptor by hand.
CREATE TABLE project_observed_policies (
  scope TEXT NOT NULL,
  runner_id TEXT NOT NULL,
  policy_id TEXT NOT NULL,
  descriptor_json TEXT NOT NULL CHECK (json_valid(descriptor_json)),
  observed_at TEXT NOT NULL,
  PRIMARY KEY (scope, runner_id)
);

-- +goose Down
DROP TABLE project_observed_policies;
