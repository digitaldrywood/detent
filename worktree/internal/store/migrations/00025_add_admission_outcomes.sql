-- +goose Up
ALTER TABLE backlog_admission_proposals
ADD COLUMN decision_comment_id TEXT;

ALTER TABLE backlog_admission_proposals
ADD COLUMN decision_actor_login TEXT;

ALTER TABLE backlog_admission_proposals
ADD COLUMN decision_actor_kind TEXT;

ALTER TABLE backlog_admission_proposals
ADD COLUMN transition_at TEXT;

ALTER TABLE backlog_admission_proposals
ADD COLUMN decision_seconds INTEGER;

CREATE TABLE backlog_admission_downstream_outcomes (
  proposal_id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  completed_at TEXT,
  rework_count INTEGER NOT NULL DEFAULT 0,
  review_churn_count INTEGER NOT NULL DEFAULT 0,
  spend_usd REAL NOT NULL DEFAULT 0.0,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (proposal_id) REFERENCES backlog_admission_proposals(id) ON DELETE CASCADE
);

CREATE INDEX backlog_admission_downstream_project_idx
ON backlog_admission_downstream_outcomes(project_id, updated_at DESC, proposal_id);

-- +goose Down
DROP TABLE IF EXISTS backlog_admission_downstream_outcomes;

ALTER TABLE backlog_admission_proposals
DROP COLUMN decision_seconds;

ALTER TABLE backlog_admission_proposals
DROP COLUMN transition_at;

ALTER TABLE backlog_admission_proposals
DROP COLUMN decision_actor_kind;

ALTER TABLE backlog_admission_proposals
DROP COLUMN decision_actor_login;

ALTER TABLE backlog_admission_proposals
DROP COLUMN decision_comment_id;
