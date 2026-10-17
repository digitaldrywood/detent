-- +goose Up
-- Who is executing a run (decisions section 18.12).
--
-- A queued run has two possible executors: the person who opens the exec
-- channel for it, and the hub itself handing it to the runner that already
-- holds the workspace. Both are legitimate -- the first is a reader watching
-- the output live, the second is what makes a run requested through the API
-- with no browser attached actually run -- and they can be asked for the same
-- row at the same moment.
--
-- claimed_by is what makes that race safe. It is written in the same
-- transaction that moves the row out of queued, so the first claim wins and
-- the second is refused with already_running rather than starting a second
-- process in the same worktree. It also names the executor, which is what an
-- operator reading a run needs: "relayconn_...:N" is a person's own stream,
-- "relayhub:N" is the hub's own dispatch, and "runner:..." is a run the runner
-- started itself for run_on_worktree_creation. Those are three different
-- stories when a run goes wrong.
ALTER TABLE project_action_runs ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';

-- The dispatch reads queued runs oldest first, so the grace period applies to
-- the run that has waited longest rather than to whichever the planner found.
CREATE INDEX project_action_runs_queued_idx
  ON project_action_runs(status, created_at)
  WHERE status = 'queued';

-- +goose Down
DROP INDEX project_action_runs_queued_idx;
ALTER TABLE project_action_runs DROP COLUMN claimed_by;
