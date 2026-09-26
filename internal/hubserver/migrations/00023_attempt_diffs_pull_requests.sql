-- +goose Up
-- Stored attempt diffs (decisions section 18.5).
--
-- A diff is one generation of one attempt's worktree. The generation is
-- {source, id, seq}: an attempt producer writes source 'attempt' with an empty
-- id and the run event sequence the diff belongs to; a workspace producer
-- (section 18.1, not yet implemented) writes source 'workspace' with its own
-- id and its own monotonic counter. The two live side by side and never
-- overwrite each other, which is why the generation key carries the source.
--
-- Every row names the producer that wrote it and the lease that fenced the
-- write. The producer is not necessarily the subject attempt: while an attempt
-- runs its own lease is the producer, and after it finishes only a workspace
-- lease on that attempt may write. Keeping the tuple on the row means a later
-- reader can see which generation produced what it is looking at, the same way
-- native_attempts records the lease that produced a run event.
--
-- attempt_id carries no foreign key for the same reason attempt_usage does not:
-- a diff is posted before the run event that references it, so it can arrive on
-- a checkpoint the attempt recorder has not ordered yet.
CREATE TABLE attempt_diffs (
  id TEXT PRIMARY KEY,
  attempt_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  source TEXT NOT NULL CHECK (source IN ('attempt', 'workspace')),
  source_id TEXT NOT NULL DEFAULT '',
  seq INTEGER NOT NULL CHECK (seq > 0),
  base_sha TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  producer_kind TEXT NOT NULL CHECK (producer_kind IN ('attempt', 'workspace')),
  producer_id TEXT NOT NULL,
  producer_runner_id TEXT NOT NULL,
  producer_lease_id TEXT NOT NULL,
  producer_fencing_token INTEGER NOT NULL CHECK (producer_fencing_token > 0),
  file_count INTEGER NOT NULL DEFAULT 0 CHECK (file_count >= 0),
  patch_bytes INTEGER NOT NULL DEFAULT 0 CHECK (patch_bytes >= 0),
  posted_bytes INTEGER NOT NULL DEFAULT 0 CHECK (posted_bytes >= 0),
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  created_at TEXT NOT NULL,
  -- One row per generation. A replayed post of a seq already stored is refused
  -- as stale_generation before it reaches the constraint; the constraint is
  -- what makes that impossible to get wrong.
  UNIQUE (attempt_id, source, source_id, seq)
);
-- The default read is "the latest attempt-produced diff of this attempt", and
-- ?at=<seq> is the same index seeking one generation.
CREATE INDEX attempt_diffs_generation_idx ON attempt_diffs(attempt_id, source, seq DESC);
-- The issue read rule resolves a diff through its work item, so the listing
-- side needs the scope columns to be indexed together.
CREATE INDEX attempt_diffs_item_idx ON attempt_diffs(organization_id, project_id, work_item_id, created_at);

-- One file of one stored diff, in the order the producer reported it.
--
-- patch is the unified diff for the file and is empty for three reasons that
-- the row distinguishes: binary content (binary = 1), a path the files
-- denylist refuses (denied = 1, counts intact, section 18.4), or a patch that
-- exceeded the per-file cap and was cut (truncated = 1). The filter runs on
-- write, so a denied patch is never stored, not merely never served: a later
-- reader is not shown what the live reader was not.
CREATE TABLE attempt_diff_files (
  diff_id TEXT NOT NULL REFERENCES attempt_diffs(id) ON DELETE CASCADE,
  position INTEGER NOT NULL CHECK (position >= 0),
  path TEXT NOT NULL,
  old_path TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('added', 'modified', 'deleted', 'renamed')),
  additions INTEGER NOT NULL DEFAULT 0 CHECK (additions >= 0),
  deletions INTEGER NOT NULL DEFAULT 0 CHECK (deletions >= 0),
  binary INTEGER NOT NULL DEFAULT 0 CHECK (binary IN (0, 1)),
  patch TEXT NOT NULL DEFAULT '',
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  denied INTEGER NOT NULL DEFAULT 0 CHECK (denied IN (0, 1)),
  CHECK (denied = 0 OR patch = ''),
  PRIMARY KEY (diff_id, position)
);

-- Pull request actions (decisions section 18.6).
--
-- An action is not executed by the hub. It is a native issue the existing
-- merge queue claims on a runner with a checkout, so this table is only the
-- association between that issue and what it was asked to do.
--
-- The action's own idempotency lives in native_commands, like every other
-- mutation, so this table carries no key: a replayed post answers with the
-- first call's work item instead of opening a second one.
--
-- expected_head_sha is what the actor believed the head was. It is recorded
-- rather than only compared, so a runner that picks the action up later can
-- refuse a head that moved between acceptance and execution exactly as the
-- endpoint refuses one that moved before acceptance.
CREATE TABLE pull_request_actions (
  id TEXT PRIMARY KEY,
  work_item_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  subject_work_item_id TEXT NOT NULL,
  action TEXT NOT NULL CHECK (action IN ('open', 'update_branch', 'merge')),
  number INTEGER NOT NULL DEFAULT 0 CHECK (number >= 0),
  expected_head_sha TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  closed_at TEXT,
  FOREIGN KEY (organization_id, project_id, subject_work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
-- The subject issue's panel lists the actions it has open.
CREATE INDEX pull_request_actions_subject_idx ON pull_request_actions(organization_id, project_id, subject_work_item_id, created_at);
-- An action resolves from the issue the merge queue handed the runner.
CREATE UNIQUE INDEX pull_request_actions_item_idx ON pull_request_actions(work_item_id);

-- work_item_revision is the issues.revision an attempt ran against, captured
-- at run.started. The work item change surface reports the highest revision a
-- succeeded attempt recorded a change for, so a reader can tell a change that
-- covers the item as it stands from one that answered an older version. It
-- defaults to 0, which is below every real revision, so attempts recorded
-- before this migration never claim to cover anything.
ALTER TABLE native_attempts ADD COLUMN work_item_revision INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE native_attempts DROP COLUMN work_item_revision;
DROP TABLE pull_request_actions;
DROP TABLE attempt_diff_files;
DROP TABLE attempt_diffs;
