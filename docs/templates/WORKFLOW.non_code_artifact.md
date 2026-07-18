# Non-Code Artifact Workflow

You are working on a local production work item, not a GitHub issue-to-PR task.
Use the filesystem workspace and configured output directory. Do not require a
git branch, pull request, CI run, or merge train unless the work item explicitly
asks for one.

Maintain the local `## Codex Workpad` record through Detent events. Every
Workpad update must include one `detent-status` fenced block. Detent reads
blocker and human-action declarations from that block; narrative sentences are
never read as blockers. `status` must be one of `in_progress`, `blocked`, or
`complete`.

Use `in_progress` while production or validation is still active:

```detent-status
schema: 1
status: in_progress
blockers: []
human_action: null
```

Use `complete` only when the artifact manifest is written, local validation is
green, and no actionable review feedback remains:

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

Use `blocked` when required source assets, credentials, or human-only decisions
are missing:

```detent-status
schema: 1
status: blocked
blockers: []
human_action: "Provide the missing source assets."
```

Read the work item title, description, fields, metadata, and deliverable data.
Use the project source folder for instructions, scripts, media assets, product
copy, and production constraints. If required source assets are missing, record
the missing inputs clearly in the output manifest and set `render_status` to
`missing_assets` through the local status store or handoff process.

Produce a machine-readable artifact manifest under the work item output
directory. For video ad production, include:

- work item id and external id
- source asset paths used
- generated script or storyboard path
- render instructions or render output paths
- preview or review URL when available
- validation status and validation notes
- next external-system action

If meaningful out-of-scope work is discovered, file a separate tracker issue in Backlog with a best-guess `detent-agent` effort block instead of expanding the current work item.

## Required Execution Flow

This workflow uses the artifact autopilot handoff: `agent.auto_promote.enabled:
true`, `quiet_seconds: 0`, and `gate_wait_state: source`. Completed agents keep
the work item in `Production`, set the Workpad `detent-status` block to
`status: complete`, set `render_status` to `valid` when the artifact gate is
satisfied, and let Detent promote the item to `Ready for Pickup`. Do not
self-move work items to `Review`.

If a delivery flow uses a rebase, capture the branch's effective diff against
its merge base or preserve the pre-rebase ref first. After the rebase, compare
with `git range-diff` or an equivalent diff-stat and confirm the same files and
hunks remain. If changes are missing without explanation or conflict resolution
dropped hunks, stop before pushing and move the work item to the configured
blocked or exception state.

### For Todo

1. Move the work item to `Production`.
2. Read the work item title, description, fields, metadata, and deliverable
   data.
3. Produce the artifact manifest under the configured output directory.
4. When the artifact is ready and local validation passes, set `render_status`
   to `valid`, update the Workpad block to `status: complete` with
   `blockers: []` and `human_action: null`, leave the work item in
   `Production`, and do not move it to `Review`.

### For Production

Continue production from the current filesystem state. When the artifact is
ready and local validation passes, set `render_status` to `valid`, set the
Workpad block to `status: complete`, and leave the work item in `Production`.

### For Rework

Move the work item to `Production`, address the requested changes, rerun the
artifact validation gate, set `render_status` to `valid`, set the Workpad block
to `status: complete`, and do not move the work item to `Review`.

### For Review

Review is reserved for explicit human opt-out or gate-wait timeout. Re-read the
feedback, update the artifact, then follow the Rework flow. Use `recut`,
`invalid`, or `missing_assets` when the item needs rework.
