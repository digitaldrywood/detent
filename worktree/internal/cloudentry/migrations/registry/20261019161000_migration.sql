-- +goose Up
CREATE TABLE platform_notifications (
  event TEXT NOT NULL CHECK (event IN ('signup', 'organization')),
  entity_id TEXT NOT NULL,
  email TEXT NOT NULL,
  name TEXT NOT NULL,
  plan TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  attempted_at TEXT,
  PRIMARY KEY (event, entity_id)
);
CREATE INDEX platform_notifications_pending ON platform_notifications(occurred_at) WHERE attempted_at IS NULL;
