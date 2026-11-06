-- +goose Up
CREATE TABLE repositories (
  id INTEGER PRIMARY KEY,
  github_node_id TEXT NOT NULL UNIQUE,
  github_database_id INTEGER UNIQUE,
  github_owner TEXT NOT NULL,
  github_name TEXT NOT NULL,
  github_installation_id INTEGER,
  config_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(config_json)),
  webhook_cursor TEXT,
  reconcile_cursor TEXT,
  last_webhook_at TEXT,
  last_reconciled_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (github_owner, github_name)
);

CREATE TABLE workflow_states (
  id INTEGER PRIMARY KEY,
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  github_node_id TEXT UNIQUE,
  source_name TEXT NOT NULL,
  detent_state TEXT NOT NULL,
  terminal INTEGER NOT NULL DEFAULT 0 CHECK (terminal IN (0, 1)),
  dispatchable INTEGER NOT NULL DEFAULT 0 CHECK (dispatchable IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (repository_id, source_name)
);

CREATE TABLE issues (
  id INTEGER PRIMARY KEY,
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  workflow_state_id INTEGER REFERENCES workflow_states(id) ON DELETE SET NULL,
  github_node_id TEXT NOT NULL UNIQUE,
  github_database_id INTEGER UNIQUE,
  github_number INTEGER NOT NULL CHECK (github_number > 0),
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL,
  github_state TEXT NOT NULL,
  labels_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(labels_json)),
  assignees_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(assignees_json)),
  source_version TEXT NOT NULL,
  source_updated_at TEXT NOT NULL,
  synchronized_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (repository_id, github_number)
);

CREATE TABLE issue_dependencies (
  blocker_issue_id INTEGER NOT NULL REFERENCES issues(id),
  dependent_issue_id INTEGER NOT NULL REFERENCES issues(id),
  provenance TEXT NOT NULL,
  source_version TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (blocker_issue_id, dependent_issue_id),
  CHECK (blocker_issue_id <> dependent_issue_id)
);

CREATE INDEX issue_dependencies_dependent_idx
ON issue_dependencies(dependent_issue_id, blocker_issue_id);

CREATE TABLE queue_entries (
  id INTEGER PRIMARY KEY,
  issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  workflow_state_id INTEGER REFERENCES workflow_states(id) ON DELETE SET NULL,
  scope TEXT NOT NULL,
  state TEXT NOT NULL,
  rank TEXT NOT NULL,
  priority_override INTEGER,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (scope, issue_id),
  UNIQUE (scope, state, rank)
);

CREATE INDEX queue_entries_order_idx
ON queue_entries(scope, state, priority_override, rank, issue_id);

CREATE TABLE pull_requests (
  id INTEGER PRIMARY KEY,
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL,
  github_node_id TEXT NOT NULL UNIQUE,
  github_database_id INTEGER UNIQUE,
  github_number INTEGER NOT NULL CHECK (github_number > 0),
  title TEXT NOT NULL,
  url TEXT NOT NULL,
  github_state TEXT NOT NULL,
  draft INTEGER NOT NULL DEFAULT 0 CHECK (draft IN (0, 1)),
  head_ref TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  base_ref TEXT NOT NULL,
  base_sha TEXT NOT NULL DEFAULT '',
  mergeable_state TEXT NOT NULL DEFAULT '',
  checks_summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(checks_summary_json)),
  reviews_summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(reviews_summary_json)),
  merge_ready INTEGER NOT NULL DEFAULT 0 CHECK (merge_ready IN (0, 1)),
  merge_readiness_refreshed_at TEXT,
  source_version TEXT NOT NULL,
  source_updated_at TEXT NOT NULL,
  synchronized_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (repository_id, github_number)
);

CREATE INDEX pull_requests_issue_idx
ON pull_requests(issue_id, github_number);
