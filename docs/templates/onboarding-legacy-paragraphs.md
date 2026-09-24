### For In Progress

### For Merging

### For Production

### For Review

### For Rework

### For Todo

1. Confirm `$go-workflow:ship` is available in the Codex environment. If it is
   unavailable, keep the issue in `Merging` and record the missing ship workflow
   as `human_action` in the `detent-status` block.
2. Invoke and follow `$go-workflow:ship`.
3. Do not call `gh pr merge` directly outside the ship workflow.
4. End with exactly one terminal outcome:
   - pull request merged and issue moved to `Done`;
   - issue moved to `Rework` with an actionable defect;
   - issue remains in `Merging` with a concrete external blocker recorded in
     the `detent-status` block and described in the `## Codex Workpad`.
5. Move the issue to `Done` only after the pull request is merged.

1. Create GitHub's native `blocked_by` dependency relation.

1. Move the issue to `In Progress`.
2. Create or update the persistent `## Codex Workpad` comment with the plan,
   acceptance criteria, validation plan, and the `in_progress`
   `detent-status` block shown above.
3. Fetch current `origin/main`, confirm this worktree is based on it, and
   confirm every native dependency relation, `detent-status` blocker, and
   issue-body `Depends on:` reference is merged or otherwise terminal before
   coding.
4. Reproduce or confirm the reported behavior before changing code when the
   issue is a bug.
5. Implement the smallest complete change that satisfies the issue.
6. Run focused tests for touched packages, then run the configured validation
   gate.
7. Commit and push the branch.
8. Open or update a pull request that references the issue.
9. Re-check pull request comments, inline review comments, and CI after the
   latest push.
10. If the PR is open, not a draft, references the issue, validation is green,
    and no actionable review comments remain, leave the issue in `In Progress`,
    update the Workpad block to `status: complete` with `blockers: []` and
    `human_action: null`, and do not move the issue to `Human Review`.

1. Move the issue to `In Progress`.
2. Create or update the persistent `## Codex Workpad` comment with the plan,
   acceptance criteria, validation plan, and the `in_progress`
   `detent-status` block shown above.
3. Fetch current `origin/main`, confirm this worktree is based on it, and
   confirm every native dependency relation, `detent-status` blocker, and
   issue-body `Depends on:` reference is merged or otherwise terminal before
   coding.
4. Reproduce or confirm the reported behavior before changing code when the
   issue is a bug.
5. Implement the smallest complete change that satisfies the issue.
6. Run focused tests for touched packages, then run the configured validation
   gate.
7. Commit and push the branch.
8. Open or update a pull request that references the issue.
9. Re-check pull request comments, inline review comments, and CI after the
   latest push.
10. Move the issue to `Human Review` only after the pull request is open, not a
    draft, references the issue, validation is green, and no actionable review
    comments remain.

1. Move the issue to `In Progress`.
2. Fetch current `origin/main`, confirm this worktree is based on it, and
   confirm every native dependency relation, `detent-status` blocker, and
   issue-body `Depends on:` reference is merged or otherwise terminal before
   coding.
3. Reproduce or confirm the reported behavior before changing code when the
   issue is a bug.
4. Implement the smallest complete change that satisfies the issue.
5. Run focused tests for touched packages, then run the configured validation
   gate.
6. Commit and push the branch.
7. Open or update a pull request that references the issue.
8. Move the issue to `Human Review` only after local validation passes and the
   PR is ready.

1. Move the work item to `Production`.
2. Read the work item title, description, fields, metadata, and deliverable
   data.
3. Produce the artifact manifest under the configured output directory.
4. When the artifact is ready and local validation passes, set `render_status`
   to `valid`, update the Workpad block to `status: complete` with
   `blockers: []` and `human_action: null`, leave the work item in
   `Production`, and do not move it to `Review`.

1. Move the work item to `Production`.
2. Read the work item title, description, fields, metadata, and deliverable
   data.
3. Produce the artifact manifest under the configured output directory.
4. When the artifact is ready for review, set `render_status` to
   `pending_review`, update the Workpad block to `status: complete` with
   `blockers: []` and `human_action: null`, and move the work item to
   `Review`.

1. Re-read all human and bot feedback.
2. Move the issue to `In Progress`.
3. Fix the requested changes.
4. Push updates to the pull request.
5. Run the full pre-review gate again.
6. Move the issue back to `Human Review` only when the gate passes.

1. Re-read all human and bot feedback.
2. Move the issue to `In Progress`.
3. Fix the requested changes.
4. Push updates to the pull request.
5. Run the full pre-review gate again.
6. When the gate passes, leave the issue in `In Progress`, update the Workpad
   block to `status: complete`, and do not move the issue to `Human Review`.

1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including
   the `detent-status` block.
2. Continue from the current repository and tracker state.
3. If implementation is complete, run the full pre-review gate, update the
   Workpad block to `status: complete` with `blockers: []` and
   `human_action: null`, and move the issue to `Human Review` only when the
   gate passes.

1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including
   the `detent-status` block.
2. Continue from the current repository and tracker state.
3. If implementation is complete, run the full pre-review gate, update the
   Workpad block to `status: complete` with `blockers: []` and
   `human_action: null`, leave the issue in `In Progress`, and do not move the
   issue to `Human Review`.

2. Declare the blocker in the Workpad status block with `status: blocked`.

3. Always preserve a machine-readable issue-body line such as
   `Depends on: owner/repo#123`, alongside the native relation when supported.
   Workpad mentions alone are insufficient.

Before any rebase, capture the branch's effective diff against its merge base
or preserve the pre-rebase ref. After the rebase, compare with `git range-diff`
or an equivalent diff-stat and confirm the same files and hunks remain. If
changes are missing without explanation or conflict resolution dropped hunks,
stop before pushing and move the issue to the configured blocked or exception
state.

Continue implementation from the current repository state. If implementation is
complete, run the validation gate, update the Workpad block to
`status: complete` with `blockers: []` and `human_action: null`, push the PR
branch, and move the issue to `Human Review`.

Continue production from the current filesystem state. When the artifact is
ready and local validation passes, set `render_status` to `valid`, set the
Workpad block to `status: complete`, and leave the work item in `Production`.

Continue production from the current filesystem state. When the artifact is
ready for review, set `render_status` to `pending_review`, set the Workpad
block to `status: complete`, and move the work item to `Review`.

Do not continue production unless feedback asks for changes. When a human or
external renderer marks `render_status` as `approved` or `valid`, Detent can
promote the item to `Ready for Pickup`.

Follow repository instructions, keep changes scoped to the issue, and keep a
single persistent `## Codex Workpad` issue comment updated with the plan,
validation evidence, and final handoff. Every Workpad update must include one
`detent-status` fenced block. Detent reads blocker and human-action
declarations from that block; narrative sentences are never read as blockers.
`status` must be one of `in_progress`, `blocked`, or `complete`.

Follow repository instructions, keep changes scoped to the issue, and keep the
local `## Codex Workpad` record updated through Detent events. Every Workpad
update must include one `detent-status` fenced block. Detent reads blocker and
human-action declarations from that block; narrative sentences are never read as
blockers. GitHub issue comments, labels, Projects, issue fields, and issue close
state must remain untouched. Pull request comments and merges created by Detent
are allowed for Detent-owned PR lifecycle work. `status` must be one of
`in_progress`, `blocked`, or `complete`.

For a real human need, finish independent work and authorized fallbacks
first. Record a concrete `human_action` in a structured Workpad with
`status: blocked`. Detent moves the card to Blocked and shows "Needs you".
Keep the PR intact. Do not create another issue, label, or dependency for
clarification or approval. A reply authorizes only what it actually says;
retain separate external-action approvals. Infrastructure failures remain
instance-owned. Never acknowledge an independent breaker park. Intentional
standalone human work and tracking epics remain non-executable.

For dependency blockers in this local-status workflow, declare the blocker in
the Workpad status block with `status: blocked`:

For dependency blockers, use this order:

If a delivery flow uses a rebase, capture the branch's effective diff against
its merge base or preserve the pre-rebase ref first. After the rebase, compare
with `git range-diff` or an equivalent diff-stat and confirm the same files and
hunks remain. If changes are missing without explanation or conflict resolution
dropped hunks, stop before pushing and move the work item to the configured
blocked or exception state.

If a human explicitly authorizes upstream GitHub dependency metadata writes
outside this read-only local-status workflow, prefer GitHub's native
`blocked_by` dependency relation:

If meaningful out-of-scope work is discovered, file a separate tracker issue in Backlog with a best-guess `detent-agent` effort block instead of expanding the current work item.

Legacy fallback during the deprecation window: if a human explicitly authorizes
upstream GitHub metadata writes, native dependencies are unavailable, and the
project has not migrated, keep a machine-readable issue-body line such as
`Blocked by: #123` or `Depends on: owner/repo#123`.

Maintain the local `## Codex Workpad` record through Detent events. Every
Workpad update must include one `detent-status` fenced block. Detent reads
blocker and human-action declarations from that block; narrative sentences are
never read as blockers. `status` must be one of `in_progress`, `blocked`, or
`complete`.

Move the work item to `Production`, address the requested changes, rerun the
artifact validation gate, set `render_status` to `pending_review`, set the
Workpad block to `status: complete`, and move the work item back to `Review`.

Move the work item to `Production`, address the requested changes, rerun the
artifact validation gate, set `render_status` to `valid`, set the Workpad block
to `status: complete`, and do not move the work item to `Review`.

Read all review feedback, fix the requested changes, rerun validation, push the
branch, and move the issue back to `Human Review` only when the gate passes.

Rebase onto current `origin/main`, rerun the configured validation gate, push,
watch current-head CI, merge with the configured PR workflow once green, and
move the issue to `Done`. If an external blocker remains, keep the issue in
`Merging` and record the exact blocker in the `detent-status` block.

Review is reserved for explicit human opt-out or gate-wait timeout. Re-read the
feedback, update the artifact, then follow the Rework flow.

Review is reserved for explicit human opt-out or gate-wait timeout. Re-read the
feedback, update the artifact, then follow the Rework flow. Use `recut`,
`invalid`, or `missing_assets` when the item needs rework.

The following applies only to genuine software dependencies.

This workflow uses the artifact autopilot handoff: `agent.auto_promote.enabled:
true`, `quiet_seconds: 0`, and `gate_wait_state: source`. Completed agents keep
the work item in `Production`, set the Workpad `detent-status` block to
`status: complete`, set `render_status` to `valid` when the artifact gate is
satisfied, and let Detent promote the item to `Ready for Pickup`. Do not
self-move work items to `Review`.

This workflow uses the autopilot handoff: `agent.auto_promote.enabled: true`,
`quiet_seconds: 0`, and `gate_wait_state: source`. Completed agents leave
issues in the active lane, set the Workpad `detent-status` block to
`status: complete`, and let Detent promote eligible issues to `Merging`
when the PR gate is green. Do not self-move issues to `Human Review`.

This workflow uses the review-gate handoff: completed agents move issues to
`Human Review` after the PR gate is ready, and a human or quiet-period
auto-promote advances eligible issues to `Merging`.

This workflow uses the review-gate handoff: completed artifact work moves to
`Review`, and a human or external renderer marks the artifact `approved` or
`valid` before Detent promotes it to `Ready for Pickup`.

Use `blocked` when required source assets, credentials, or human-only decisions
are missing:

Use `complete` only when the artifact manifest is written, local validation is
green, and no actionable review feedback remains:

Use `complete` only when the pull request is open, marked ready for review,
is not a draft, references the issue, validation is green, and no actionable
review comments remain. If the pull request is a draft, mark it ready and
verify the resulting state before declaring completion:

Use `in_progress` while implementation or validation is still active:

Use `in_progress` while production or validation is still active:

Use the current Detent state as the source of truth for which section applies.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

```detent-status
schema: 1
status: blocked
blockers: []
human_action: "Provide the missing source assets."
```

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

```detent-status
schema: 1
status: in_progress
blockers: []
human_action: null
```

```sh
BLOCKED_NUMBER=<blocked-issue-number>
BLOCKER_NUMBER=<blocker-issue-number>
BLOCKER_ID="$(gh api repos/{owner}/{repo}/issues/$BLOCKER_NUMBER --jq '.id')"
gh api --method POST "repos/{owner}/{repo}/issues/$BLOCKED_NUMBER/dependencies/blocked_by" -F issue_id="$BLOCKER_ID"
```

```sh
gh pr ready <number>
gh pr view <number> --json isDraft --jq '.isDraft' # must be false
```
