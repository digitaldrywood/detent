-- +goose Up
-- One terminal stream, recorded (decisions section 18.3).
--
-- workspaces.terminal.record is on by default and section 18.3 stores "each
-- stream as an asciicast v2 artifact referenced from the relay session row".
-- Each stream, not each connection: a terminal stream is never shared, so every
-- open is its own PTY and its own recording, and one tab that opened three
-- shells leaves three.
--
-- The bytes live in the hub rather than in the artifact service: a bounded,
-- audience-scoped text blob the hub keeps, where the cap is what makes keeping
-- it safe. The audience is why this is a table of its own rather than a column
-- on the relay session row. A terminal recording's audience is narrower than
-- the issue's -- "the person who ran the session
-- plus owners and admins, never the issue's readers" -- and a `user`-isolation
-- recording is narrower again, readable by owners only. Those are facts about
-- one stream, so they are stored beside it and checked when it is read.
--
-- A recording can carry what the runner account can see, which is wider than
-- the issue. That is the whole reason for the narrowing, and it is also why the
-- isolation level is stored rather than re-derived: the level the PTY actually
-- ran at is what decides who may read it, and an organization that changes the
-- setting afterwards must not widen the audience of a recording already made.
CREATE TABLE workspace_terminal_recordings (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  -- relay_session_id is the workspace_relay_sessions row this stream belonged
  -- to, which is the reference section 18.3 asks for. It is not a foreign key
  -- because the audit row is written outside the hub's transaction helper --
  -- an audit that vanished with a rolled-back write would not be an audit --
  -- and a recording must survive a torn connection for the same reason.
  relay_session_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,
  -- principal_id and subject are the person who ran it, which is half the
  -- audience: they may read their own recording whatever their role.
  principal_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  isolation TEXT NOT NULL CHECK (isolation IN ('user', 'container')),
  -- support_reason is recorded for a support actor's session, because section
  -- 18.3 lets one have a terminal only with a reason recorded and only at
  -- container isolation. A recording with no reason on it was not a support
  -- session's.
  support_reason TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  terminal_cols INTEGER NOT NULL CHECK (terminal_cols > 0),
  terminal_rows INTEGER NOT NULL CHECK (terminal_rows > 0),
  -- cast_text holds the asciicast v2 document: one JSON header line, then one JSON
  -- array per event. Input events are marked "i" and output events "o", so a
  -- reader can tell what the person typed from what the shell answered --
  -- section 18.3 is explicit that recording cannot remove what the person
  -- typed, and marking it is the honest alternative to dropping it.
  cast_text TEXT NOT NULL DEFAULT '',
  cast_bytes INTEGER NOT NULL DEFAULT 0 CHECK (cast_bytes >= 0),
  -- truncated marks a recording that reached its cap. A terminal has no output
  -- cap on the wire -- a shell that stopped producing after a megabyte would be
  -- a terminal that stopped working -- so the cap lives here, on the stored
  -- copy, and a reader is told when the copy is shorter than the session was.
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- One recording per stream. A stream is never shared and never reused, so this
-- is also what makes the write idempotent: a teardown path that runs twice
-- updates the row it already wrote rather than leaving two half-recordings of
-- the same shell.
CREATE UNIQUE INDEX workspace_terminal_recordings_stream_idx
  ON workspace_terminal_recordings(workspace_id, stream_id);

-- The listing reads a workspace's recordings newest first, and the relay
-- session's own listing narrows by the connection.
CREATE INDEX workspace_terminal_recordings_workspace_idx
  ON workspace_terminal_recordings(workspace_id, started_at DESC);
CREATE INDEX workspace_terminal_recordings_session_idx
  ON workspace_terminal_recordings(relay_session_id, started_at DESC);

-- +goose Down
DROP INDEX workspace_terminal_recordings_session_idx;
DROP INDEX workspace_terminal_recordings_workspace_idx;
DROP INDEX workspace_terminal_recordings_stream_idx;
DROP TABLE workspace_terminal_recordings;
