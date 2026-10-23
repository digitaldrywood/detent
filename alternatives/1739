-- +goose Up
CREATE TABLE machines (
  id TEXT PRIMARY KEY,
  hostname TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  capabilities_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(capabilities_json)),
  capacity INTEGER NOT NULL CHECK (capacity >= 0),
  version TEXT NOT NULL,
  last_heartbeat_at TEXT NOT NULL,
  registered_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX machines_heartbeat_idx
ON machines(last_heartbeat_at, id);

CREATE TABLE leases (
  fencing_token INTEGER PRIMARY KEY AUTOINCREMENT,
  lease_id TEXT NOT NULL UNIQUE,
  issue_id INTEGER NOT NULL REFERENCES issues(id),
  machine_id TEXT NOT NULL REFERENCES machines(id),
  session_id TEXT NOT NULL UNIQUE,
  expires_at TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  renewed_at TEXT NOT NULL,
  released_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX leases_active_issue_idx
ON leases(issue_id)
WHERE released_at IS NULL;

CREATE INDEX leases_expiry_idx
ON leases(released_at, expires_at, issue_id);

CREATE TABLE work_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id INTEGER NOT NULL REFERENCES issues(id),
  fencing_token INTEGER REFERENCES leases(fencing_token),
  machine_id TEXT REFERENCES machines(id),
  session_id TEXT,
  run_id TEXT,
  kind TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json)),
  occurred_at TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);

CREATE INDEX work_events_issue_idx
ON work_events(issue_id, id);

CREATE INDEX work_events_session_idx
ON work_events(session_id, id);

-- +goose StatementBegin
CREATE TRIGGER work_events_no_update
BEFORE UPDATE ON work_events
BEGIN
  SELECT RAISE(ABORT, 'work events are append-only');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER work_events_no_delete
BEFORE DELETE ON work_events
BEGIN
  SELECT RAISE(ABORT, 'work events are append-only');
END;
-- +goose StatementEnd
