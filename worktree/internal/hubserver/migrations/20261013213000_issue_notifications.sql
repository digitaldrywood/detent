-- +goose Up
ALTER TABLE hosted_members ADD COLUMN notify_issues INTEGER NOT NULL DEFAULT 1 CHECK (notify_issues IN (0, 1));

CREATE TABLE issue_notification_cursors (
  work_item_id TEXT PRIMARY KEY REFERENCES issues(native_id) ON DELETE CASCADE,
  sequence INTEGER NOT NULL
);
INSERT INTO issue_notification_cursors (work_item_id, sequence) SELECT native_id, event_sequence FROM issues;

CREATE TABLE issue_subscriptions (
  work_item_id TEXT NOT NULL REFERENCES issues(native_id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES hosted_members(user_id) ON DELETE CASCADE,
  token TEXT NOT NULL UNIQUE,
  unsubscribed INTEGER NOT NULL DEFAULT 0 CHECK (unsubscribed IN (0, 1)),
  PRIMARY KEY (work_item_id, user_id)
);

CREATE TABLE issue_notification_batches (
  work_item_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  due_at TEXT NOT NULL,
  notices_json TEXT NOT NULL CHECK (json_valid(notices_json)),
  PRIMARY KEY (work_item_id, user_id),
  FOREIGN KEY (work_item_id, user_id) REFERENCES issue_subscriptions(work_item_id, user_id) ON DELETE CASCADE
);
CREATE INDEX issue_notification_batches_due ON issue_notification_batches(due_at);

-- +goose Down
DROP TABLE issue_notification_batches;
DROP TABLE issue_subscriptions;
DROP TABLE issue_notification_cursors;
ALTER TABLE hosted_members DROP COLUMN notify_issues;
