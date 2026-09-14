-- +goose Up
-- Project actions and their runs (decisions section 18.12).
--
-- An action is a command the project wrote down: a name, a shell command, an
-- optional chord, and two flags. It is stored on the project rather than on an
-- issue or a workspace because that is what makes it worth writing down once --
-- every conversation in the project offers the same actions, and a worktree
-- opened tomorrow runs the same setup as one opened today.

CREATE TABLE project_actions (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  command TEXT NOT NULL,
  -- keybinding is a chord string ("mod+shift+t") or empty for no shortcut.
  -- The hub stores it and never resolves it: a keystroke is resolved in the
  -- browser against the platform it is running on, and the hub has no way to
  -- know whether "mod" means Command or Control for the person pressing it.
  keybinding TEXT NOT NULL DEFAULT '',
  -- icon names one of the six project action glyphs (play, test, lint, configure,
  -- build, debug). It is the dialog's own field and belongs with the action
  -- rather than in client-local storage, because the header button a
  -- colleague sees has to carry the glyph the author chose.
  icon TEXT NOT NULL DEFAULT 'play',
  -- preview_url and open_preview are stored and echoed, never acted on: the
  -- Browser surface is deferred (section 18.7), so the field is kept so an
  -- action authored today does not have to be re-authored when it lands.
  preview_url TEXT NOT NULL DEFAULT '',
  open_preview INTEGER NOT NULL DEFAULT 0 CHECK (open_preview IN (0, 1)),
  run_on_worktree_creation INTEGER NOT NULL DEFAULT 0 CHECK (run_on_worktree_creation IN (0, 1)),
  created_by TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);
-- The listing is per project in authoring order, which is also the order
-- run-on-worktree-creation actions run in: an author who wants install before
-- build writes install first, and nothing else in the contract would say so.
CREATE INDEX project_actions_project_idx ON project_actions(organization_id, project_id, created_at, id);
-- A chord resolves to one command, so two actions in a project may not claim
-- the same one. Without this the client would have to pick a winner, and which
-- one it picked would depend on listing order.
CREATE UNIQUE INDEX project_actions_keybinding_idx
  ON project_actions(organization_id, project_id, keybinding)
  WHERE keybinding <> '';

-- One run of one action.
--
-- The row is written before any frame reaches a runner, so a run that never
-- starts is visible as queued rather than absent, and the run id is what joins
-- the relay stream to the record the hub updates from the frames it relays.
-- output_artifact is a receipt, never the bytes: the hub stores no blobs
-- (section 18.7's artifact rule), so the output lives in the artifact service
-- with the issue's read audience and this column names it.
CREATE TABLE project_action_runs (
  id TEXT PRIMARY KEY,
  action_id TEXT NOT NULL REFERENCES project_actions(id) ON DELETE CASCADE,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL REFERENCES workspace_sessions(id) ON DELETE CASCADE,
  -- command is the action's command as it was at the moment the run started.
  -- An action that is edited afterwards must not rewrite the history of what
  -- already ran, which is what reading the command through action_id would do.
  command TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
  -- exit_code is null until the process exits. A run that failed without
  -- exiting -- a lost lease, a closed stream -- keeps it null and says so in
  -- reason instead, so "exit 0" can never be confused with "never ran".
  exit_code INTEGER,
  reason TEXT NOT NULL DEFAULT '',
  started_at TEXT,
  finished_at TEXT,
  output_artifact TEXT NOT NULL DEFAULT '',
  -- output holds the run's combined stdout and stderr, which the hub serves at
  -- .../actions/:action/runs/:run/output.
  --
  -- The hub stores these bytes itself rather than putting them through the
  -- artifact service. attempt_diffs (migration 00026) is the precedent: a
  -- bounded, already-audience-scoped text blob the hub keeps, where the exec
  -- channel's 1 MiB cap (section 18.12) is what makes keeping it safe -- the
  -- runner stops forwarding past the cap, so one column can never become
  -- unbounded storage. A run's output has exactly the issue's read audience
  -- the run does, so routing it through a blob store would buy a second
  -- audience to keep in step and nothing else.
  output TEXT NOT NULL DEFAULT '',
  output_bytes INTEGER NOT NULL DEFAULT 0 CHECK (output_bytes >= 0),
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  -- created_by is the person who asked, or empty for a run the runner started
  -- itself because the action carries run_on_worktree_creation.
  created_by TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX project_action_runs_action_idx ON project_action_runs(action_id, created_at DESC);
CREATE INDEX project_action_runs_workspace_idx ON project_action_runs(workspace_id, created_at DESC);
CREATE INDEX project_action_runs_project_idx ON project_action_runs(organization_id, project_id, created_at DESC);

-- +goose Down
DROP TABLE project_action_runs;
DROP TABLE project_actions;
