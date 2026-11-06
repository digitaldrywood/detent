-- +goose Up
ALTER TABLE project_observed_policies ADD COLUMN source_json TEXT NOT NULL DEFAULT 'null' CHECK (json_valid(source_json));
CREATE TABLE project_workflow_applies (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL,
  repository TEXT NOT NULL,
  source_commit TEXT NOT NULL,
  previous_definition_digest TEXT NOT NULL,
  definition_digest TEXT NOT NULL,
  runner_id TEXT NOT NULL,
  applied_by TEXT NOT NULL,
  applied_at TEXT NOT NULL
);
CREATE INDEX project_workflow_applies_scope ON project_workflow_applies(scope, id DESC);

-- +goose Down
DROP TABLE project_workflow_applies;
ALTER TABLE project_observed_policies DROP COLUMN source_json;
