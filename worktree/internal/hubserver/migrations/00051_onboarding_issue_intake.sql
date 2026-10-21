-- +goose Up
CREATE TABLE onboarding_issue_intake (
  project_id TEXT PRIMARY KEY REFERENCES projects(id),
  request_json TEXT NOT NULL CHECK (json_valid(request_json))
);

-- +goose Down
DROP TABLE onboarding_issue_intake;
