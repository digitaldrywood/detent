-- +goose Up
CREATE TABLE lane_ledger_instance (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  identity TEXT NOT NULL
);
INSERT INTO lane_ledger_instance (id, identity) VALUES (1, lower(hex(randomblob(16))));

CREATE TABLE lane_ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  reason TEXT NOT NULL,
  written_at TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT 'prepared'
);
CREATE INDEX lane_ledger_issue_idx ON lane_ledger(project_id, issue_id, id DESC);

CREATE TABLE lane_observations (
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  state TEXT NOT NULL,
  entered_at TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, issue_id)
);

INSERT INTO lane_ledger (project_id, issue_id, from_state, to_state, reason, written_at, result)
SELECT project_id, issue_id, from_state, to_state, reason, requested_at, tracker_result
FROM lane_mutation_receipts WHERE tracker_result IN ('prepared', 'applied') ORDER BY id;
DROP TABLE lane_mutation_receipts;

-- +goose Down
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

DROP TABLE lane_observations;
DROP TABLE lane_ledger;
DROP TABLE lane_ledger_instance;
