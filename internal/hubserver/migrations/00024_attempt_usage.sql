-- +goose Up
-- Per-attempt usage (decisions section 17, item 5).

-- attempt_usage is what one attempt spent, split by UTC hour, provider and
-- model. The runner reports the attempt's running total on run.checkpointed
-- and run.finished; the hub files only the part of that total it has not
-- already recorded, under the hour the event arrived, so spend lands in the
-- hour it happened and a redelivered event adds nothing. period is the hour
-- start as a fixed-width UTC timestamp (2006-01-02T15:00:00Z) that sorts and
-- compares as a string. attempt_id carries no foreign key because usage may
-- arrive on a checkpoint the attempt recorder did not order.
CREATE TABLE attempt_usage (
  attempt_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  period TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  input INTEGER NOT NULL DEFAULT 0 CHECK (input >= 0),
  cached_input INTEGER NOT NULL DEFAULT 0 CHECK (cached_input >= 0),
  output INTEGER NOT NULL DEFAULT 0 CHECK (output >= 0),
  cost_estimate REAL NOT NULL DEFAULT 0 CHECK (cost_estimate >= 0),
  currency TEXT NOT NULL DEFAULT 'USD',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (attempt_id, period, provider, model)
);
-- The usage report sums one organization over a period range, optionally for
-- one project.
CREATE INDEX attempt_usage_window_idx ON attempt_usage(organization_id, period, project_id);

-- +goose Down
DROP TABLE attempt_usage;
