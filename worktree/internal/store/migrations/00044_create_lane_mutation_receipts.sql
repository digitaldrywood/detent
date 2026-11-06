-- +goose Up
CREATE TABLE lane_mutation_receipts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  work_attempt_id INTEGER NOT NULL,
  generation INTEGER NOT NULL CHECK (generation > 0),
  disposition TEXT NOT NULL CHECK (disposition IN ('preserve_ownership', 'accept_completion', 'revoke_worker')),
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  reason TEXT NOT NULL,
  tracker_result TEXT NOT NULL DEFAULT 'prepared' CHECK (tracker_result IN ('prepared', 'applied', 'blocked', 'failed', 'superseded')),
  requested_at TEXT NOT NULL,
  resolved_at TEXT,
  consumed_at TEXT,
  error_message TEXT,
  FOREIGN KEY (work_attempt_id) REFERENCES work_attempts(id)
);

CREATE INDEX lane_mutation_receipts_owner_idx
ON lane_mutation_receipts(project_id, issue_id, work_attempt_id, generation, id DESC);

-- +goose Down
DROP TABLE IF EXISTS lane_mutation_receipts;
