-- +goose Up
CREATE TABLE security_audit_runs (
  id INTEGER PRIMARY KEY,
  invocation_id TEXT NOT NULL UNIQUE,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  identifier TEXT NOT NULL,
  issue_url TEXT NOT NULL,
  repository TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  base_sha TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  service_identity TEXT NOT NULL,
  reviewer_version TEXT NOT NULL,
  reviewer_digest TEXT NOT NULL,
  authentication_mode TEXT NOT NULL,
  worker_pid INTEGER NOT NULL DEFAULT 0,
  worker_pgid INTEGER NOT NULL DEFAULT 0,
  worker_started_at TEXT,
  provider_thread_id TEXT NOT NULL DEFAULT '',
  provider_session_id TEXT NOT NULL DEFAULT '',
  exit_status TEXT NOT NULL,
  failure TEXT NOT NULL DEFAULT '',
  output_digest TEXT NOT NULL DEFAULT '',
  output_bytes INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  findings_json TEXT NOT NULL DEFAULT '[]',
  attempt INTEGER NOT NULL,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);

CREATE INDEX security_audit_runs_exact_head_idx
ON security_audit_runs(project_id, repository, pr_number, base_sha, head_sha, recorded_at DESC, id DESC);

CREATE INDEX security_audit_runs_issue_idx
ON security_audit_runs(project_id, issue_id, recorded_at DESC, id DESC);

CREATE TABLE security_audit_dispositions (
  id INTEGER PRIMARY KEY,
  audit_run_id INTEGER NOT NULL REFERENCES security_audit_runs(id),
  finding_id TEXT NOT NULL,
  status TEXT NOT NULL,
  evidence TEXT NOT NULL,
  service_identity TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);

CREATE INDEX security_audit_dispositions_run_idx
ON security_audit_dispositions(audit_run_id, finding_id, recorded_at DESC, id DESC);

-- +goose StatementBegin
CREATE TRIGGER security_audit_runs_no_update
BEFORE UPDATE ON security_audit_runs
BEGIN
  SELECT RAISE(ABORT, 'security audit runs are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER security_audit_runs_no_delete
BEFORE DELETE ON security_audit_runs
BEGIN
  SELECT RAISE(ABORT, 'security audit runs are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER security_audit_dispositions_no_update
BEFORE UPDATE ON security_audit_dispositions
BEGIN
  SELECT RAISE(ABORT, 'security audit dispositions are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER security_audit_dispositions_no_delete
BEFORE DELETE ON security_audit_dispositions
BEGIN
  SELECT RAISE(ABORT, 'security audit dispositions are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS security_audit_dispositions_no_delete;
DROP TRIGGER IF EXISTS security_audit_dispositions_no_update;
DROP TRIGGER IF EXISTS security_audit_runs_no_delete;
DROP TRIGGER IF EXISTS security_audit_runs_no_update;
DROP TABLE IF EXISTS security_audit_dispositions;
DROP TABLE IF EXISTS security_audit_runs;
