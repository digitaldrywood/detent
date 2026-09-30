-- +goose Up
ALTER TABLE lease_runners ADD COLUMN isolation_policy_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(isolation_policy_json));
ALTER TABLE runner_identities ADD COLUMN backend_isolation_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(backend_isolation_json));

CREATE TABLE workspace_terminal_recordings_new (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  relay_session_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  isolation TEXT NOT NULL CHECK (isolation IN ('user', 'container', 'sandbox')),
  support_reason TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  terminal_cols INTEGER NOT NULL CHECK (terminal_cols > 0),
  terminal_rows INTEGER NOT NULL CHECK (terminal_rows > 0),
  cast_text TEXT NOT NULL DEFAULT '',
  cast_bytes INTEGER NOT NULL DEFAULT 0 CHECK (cast_bytes >= 0),
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO workspace_terminal_recordings_new SELECT * FROM workspace_terminal_recordings;
DROP TABLE workspace_terminal_recordings;
ALTER TABLE workspace_terminal_recordings_new RENAME TO workspace_terminal_recordings;
CREATE UNIQUE INDEX workspace_terminal_recordings_stream_idx
  ON workspace_terminal_recordings(workspace_id, stream_id);
CREATE INDEX workspace_terminal_recordings_workspace_idx
  ON workspace_terminal_recordings(workspace_id, started_at DESC);
CREATE INDEX workspace_terminal_recordings_session_idx
  ON workspace_terminal_recordings(relay_session_id, started_at DESC);

-- +goose Down
ALTER TABLE lease_runners DROP COLUMN isolation_policy_json;
CREATE TABLE workspace_terminal_recordings_new (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  relay_session_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  isolation TEXT NOT NULL CHECK (isolation IN ('user', 'container')),
  support_reason TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  terminal_cols INTEGER NOT NULL CHECK (terminal_cols > 0),
  terminal_rows INTEGER NOT NULL CHECK (terminal_rows > 0),
  cast_text TEXT NOT NULL DEFAULT '',
  cast_bytes INTEGER NOT NULL DEFAULT 0 CHECK (cast_bytes >= 0),
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO workspace_terminal_recordings_new SELECT * FROM workspace_terminal_recordings;
DROP TABLE workspace_terminal_recordings;
ALTER TABLE workspace_terminal_recordings_new RENAME TO workspace_terminal_recordings;
CREATE UNIQUE INDEX workspace_terminal_recordings_stream_idx
  ON workspace_terminal_recordings(workspace_id, stream_id);
CREATE INDEX workspace_terminal_recordings_workspace_idx
  ON workspace_terminal_recordings(workspace_id, started_at DESC);
CREATE INDEX workspace_terminal_recordings_session_idx
  ON workspace_terminal_recordings(relay_session_id, started_at DESC);


ALTER TABLE runner_identities DROP COLUMN backend_isolation_json;
