-- +goose Up
CREATE TABLE health_notification_states (
  identity TEXT PRIMARY KEY,
  state_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS health_notification_states;
