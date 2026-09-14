-- +goose Up
-- Where a workspace's worktree actually is (decisions section 18.13).
--
-- The header's Open picker hands the reader's operating system a
-- `cursor://file/<path>` or `vscode://file/<path>` link, and such a link only
-- reaches a worktree on the machine the browser is running on. On Detent Cloud
-- the worktree is on the customer's runner, so the client has to be able to
-- compare: it needs the host the runner reported and the absolute path it
-- prepared, and where they do not name this machine every item in the picker
-- is disabled with the host it is on rather than opening a window onto
-- nothing.
--
-- Both come from the runner's heartbeat rather than from the bind. A runner
-- that re-prepared a fresh worktree — the retention window passed, or the
-- workspace was re-requested after a hub restart — corrects the resource on
-- its next beat instead of leaving a path that no longer exists on the
-- resource a picker reads.
--
-- machine_hostname is not machine_id. The id is the hub's own binding
-- identifier and means nothing to an operating system; the hostname is what a
-- person recognises and what the disabled reason has to name.
ALTER TABLE workspace_sessions ADD COLUMN machine_hostname TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_sessions ADD COLUMN worktree_path TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE workspace_sessions DROP COLUMN worktree_path;
ALTER TABLE workspace_sessions DROP COLUMN machine_hostname;
