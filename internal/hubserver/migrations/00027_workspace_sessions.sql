-- +goose Up
-- Workspace sessions, their dispatch association, their occupancy accounting,
-- the relay's tickets and its audit rows, and the typed project event log the
-- three of them publish on (decisions sections 18.1, 18.2 and 12).

-- The typed project event stream.
--
-- Section 12's project stream carried one integer: the project's highest issue
-- event sequence, on a one-second tick, with no id and no body. A client could
-- learn that something changed and nothing about what. Section 18.1 requires
-- the opposite -- "every transition emits workspace.<state> on the project
-- event stream with the resource as data, so a client observes readiness by
-- subscription and never by polling" -- so the stream needs a durable log
-- behind it, for the same reason the conversation stream has one: a subscriber
-- is only woken, then reads from its own cursor, and a slow client therefore
-- loses nothing and reorders nothing.
--
-- seq is allocated per project inside the transaction that wrote the state the
-- event describes, so "snapshot plus events after cursor" is exact. The log is
-- swept rather than kept forever; a cursor older than the retained window is
-- answered with a re-snapshot, exactly as the conversation stream answers one.
CREATE TABLE project_events (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  seq INTEGER NOT NULL CHECK (seq > 0),
  type TEXT NOT NULL,
  -- subject_id names what the event is about (a workspace id today) so a
  -- sweep can drop a closed subject's events without parsing the body.
  subject_id TEXT NOT NULL DEFAULT '',
  data_json TEXT NOT NULL CHECK (json_valid(data_json)),
  created_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, seq)
);
CREATE INDEX project_events_sweep_idx ON project_events(created_at);

-- One workspace session.
--
-- The owner tuple is {workspace_id, runner_id, machine_id, lease_id,
-- fencing_token} and it is this workspace's own generation, never the subject
-- attempt's: a finished attempt's lease is released and never revived, so a
-- surface writing under it would be writing under a dead generation. The lease
-- is an ordinary native lease on the workspace's own work item, which is what
-- makes the runner's capacity accounting, its renewal path and its expiry
-- sweep apply to a workspace with no second mechanism.
--
-- attempt_id carries no foreign key for the same reason attempt_diffs does not:
-- a workspace may be opened on an attempt the recorder has not ordered yet, and
-- closing a workspace never deletes the attempt's artifacts.
CREATE TABLE workspace_sessions (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  -- subject_work_item_id is the issue whose worktree the workspace opens.
  subject_work_item_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL DEFAULT '',
  ref TEXT NOT NULL,
  head_sha TEXT NOT NULL DEFAULT '',
  runner_id TEXT NOT NULL DEFAULT '',
  machine_id TEXT NOT NULL DEFAULT '',
  lease_id TEXT NOT NULL DEFAULT '',
  fencing_token INTEGER NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
  state TEXT NOT NULL CHECK (state IN ('requested', 'starting', 'ready', 'idle', 'unreachable', 'closing', 'closed', 'failed')),
  reason TEXT NOT NULL DEFAULT '',
  requires_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(requires_json)),
  -- capabilities_json is null until the runner reports what it can serve; the
  -- panel reads the workspace's own capabilities once one exists, because only
  -- that runner can serve it (section 18.10).
  capabilities_json TEXT CHECK (capabilities_json IS NULL OR json_valid(capabilities_json)),
  isolation TEXT NOT NULL DEFAULT '' CHECK (isolation IN ('', 'user', 'container')),
  worktree TEXT NOT NULL DEFAULT '' CHECK (worktree IN ('', 'retained', 'fresh')),
  -- read_only is set while the subject attempt runs: the person may look at
  -- the worktree the model is editing and may not type into it.
  read_only INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0, 1)),
  idle_timeout_seconds INTEGER NOT NULL CHECK (idle_timeout_seconds > 0),
  -- expires_at is the hard lifetime cap; requested_expires_at is the separate
  -- deadline a request has to find a runner before it fails with no_runner.
  expires_at TEXT NOT NULL,
  requested_expires_at TEXT NOT NULL,
  opened_at TEXT,
  last_activity_at TEXT,
  last_heartbeat_at TEXT,
  -- rebind_deadline is set by hub restart recovery only: the original runner
  -- has until then to re-bind with its existing tuple, and a late bind is
  -- refused with stale_execution.
  rebind_deadline TEXT,
  created_by TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (organization_id, project_id, subject_work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
-- The listing filters by work item and state, and the limits count open rows
-- per organization and per person.
CREATE INDEX workspace_sessions_subject_idx ON workspace_sessions(organization_id, project_id, subject_work_item_id, created_at);
CREATE INDEX workspace_sessions_state_idx ON workspace_sessions(organization_id, state, created_at);
CREATE INDEX workspace_sessions_actor_idx ON workspace_sessions(organization_id, created_by, state);
-- One workspace per worktree at a time. A second request on the same attempt
-- while one is open is answered 409 workspace_exists with the existing id, and
-- this index is what makes that impossible to get wrong under a race. The
-- partial predicate leaves closed and failed rows out, so history accumulates
-- freely while only one row at a time may be open.
CREATE UNIQUE INDEX workspace_sessions_open_attempt_idx
  ON workspace_sessions(organization_id, project_id, attempt_id)
  WHERE attempt_id <> '' AND state NOT IN ('closed', 'failed');

-- The association between a workspace and the native issue that dispatches it.
--
-- A workspace is its own work item kind, detent:workspace, distinct from the
-- coordinator kind in section 9: it may check out the repository, it does not
-- end when a turn ends, and it stays claimed until the workspace closes. The
-- shape mirrors coordinator_items for exactly that reason -- the claim gate
-- and the dispatcher both resolve the workspace from the issue the runner was
-- handed, and nothing else about the issue says what it is for.
CREATE TABLE workspace_items (
  work_item_id TEXT NOT NULL PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  closed_at TEXT
);
CREATE UNIQUE INDEX workspace_items_workspace_idx ON workspace_items(workspace_id);
CREATE INDEX workspace_items_open_idx ON workspace_items(organization_id, project_id, closed_at);

-- Runner occupancy, written from the starting transition and ended at the
-- closing one.
--
-- The usage report's runner rows (section 17.5) add workspace_sessions and
-- workspace_seconds from this table and never from relay traffic, so double
-- counting is impossible by construction. An unreachable workspace whose lease
-- expires is ended at lease expiry rather than at the moment the hub noticed,
-- so a crashed runner does not accrue time it was not serving.
CREATE TABLE workspace_occupancy (
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  runner_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  PRIMARY KEY (workspace_id, runner_id, started_at)
);
CREATE INDEX workspace_occupancy_runner_idx ON workspace_occupancy(organization_id, runner_id, started_at);

-- Relay tickets.
--
-- Browsers cannot set headers on a WebSocket upgrade, so section 12's CSRF rule
-- is met with a ticket instead: minted by a POST that does carry the header and
-- a checked Origin, single use, 30 seconds, and bound to both the workspace and
-- the hosted session that minted it. Only the digest is stored, for the same
-- reason an API token's is: a database read must not yield a usable credential.
CREATE TABLE workspace_relay_tickets (
  digest TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  support_reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  redeemed_at TEXT
);
CREATE INDEX workspace_relay_tickets_sweep_idx ON workspace_relay_tickets(expires_at);

-- One person connection's audit row (section 18.2). Every connection writes
-- one, whatever it did and however it ended; a support actor's row carries the
-- support reason, because a surface never widens an audience silently.
CREATE TABLE workspace_relay_sessions (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  organization_id TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  connection_id TEXT NOT NULL,
  channels_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(channels_json)),
  opened_at TEXT NOT NULL,
  closed_at TEXT,
  close_reason TEXT NOT NULL DEFAULT '',
  bytes_in INTEGER NOT NULL DEFAULT 0 CHECK (bytes_in >= 0),
  bytes_out INTEGER NOT NULL DEFAULT 0 CHECK (bytes_out >= 0),
  recording_artifact TEXT NOT NULL DEFAULT '',
  support_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX workspace_relay_sessions_workspace_idx ON workspace_relay_sessions(workspace_id, opened_at);
CREATE UNIQUE INDEX workspace_relay_sessions_connection_idx ON workspace_relay_sessions(connection_id);

-- What each runner can serve, reported in the heartbeat beside the provider
-- reports (section 18.10).
--
-- It lives on runner_identities rather than in its own table for the same
-- reason provider_reports_json does: the claim gate asks "can this runner serve
-- this workspace's requires, and was it saying so recently", and the answer has
-- to come from the same row as last_heartbeat_at or freshness means nothing.
-- The default is an empty object, so a runner that has never reported serves no
-- surface and the workspace stays requested rather than being handed to a
-- runner that cannot honour it.
ALTER TABLE runner_identities ADD COLUMN workspace_capabilities_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE runner_identities ADD COLUMN workspace_isolation TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN workspace_isolation;
ALTER TABLE runner_identities DROP COLUMN workspace_capabilities_json;
DROP TABLE workspace_relay_sessions;
DROP TABLE workspace_relay_tickets;
DROP TABLE workspace_occupancy;
DROP TABLE workspace_items;
DROP TABLE workspace_sessions;
DROP TABLE project_events;
