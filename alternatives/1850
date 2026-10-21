-- +goose Up
CREATE TABLE hosted_release_preferences (
  organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES hosted_members(user_id) ON DELETE CASCADE,
  version TEXT NOT NULL,
  build_time TEXT NOT NULL,
  PRIMARY KEY (organization_id, user_id)
);

-- +goose Down
DROP TABLE hosted_release_preferences;
