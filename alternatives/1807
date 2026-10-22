-- +goose Up
CREATE TABLE hosted_work_view_preferences (
  organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES hosted_members(user_id) ON DELETE CASCADE,
  scope TEXT NOT NULL,
  view_query TEXT NOT NULL CHECK (length(view_query) <= 8192),
  PRIMARY KEY (organization_id, user_id, scope)
);

-- +goose Down
DROP TABLE hosted_work_view_preferences;
