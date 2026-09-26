-- +goose Up
-- Per-attempt usage (decisions section 17, item 5).

-- attempt_usage is what one attempt spent, split by day, provider and model.
-- The runner reports it on run.checkpointed and run.finished as the running
-- total for the attempt, so each event overwrites the row rather than adding
-- to it: a redelivered event cannot double-count. attempt_id carries no
-- foreign key because usage may arrive on a checkpoint the attempt recorder
-- did not order.
CREATE TABLE attempt_usage (
  attempt_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  day TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  input INTEGER NOT NULL DEFAULT 0 CHECK (input >= 0),
  cached_input INTEGER NOT NULL DEFAULT 0 CHECK (cached_input >= 0),
  output INTEGER NOT NULL DEFAULT 0 CHECK (output >= 0),
  cost_estimate REAL NOT NULL DEFAULT 0 CHECK (cost_estimate >= 0),
  currency TEXT NOT NULL DEFAULT 'USD',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (attempt_id, day, provider, model)
);
-- The usage report sums one organization over a day range, optionally for one
-- project.
CREATE INDEX attempt_usage_window_idx ON attempt_usage(organization_id, day, project_id);

-- +goose Down
DROP TABLE attempt_usage;
