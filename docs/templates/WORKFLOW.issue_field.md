You are working on {{ issue.identifier }}: {{ issue.title }}.
Current Detent status: {{ issue.state }}.

Follow repository instructions, keep changes scoped to the issue, and keep a
single persistent `## Codex Workpad` issue comment updated with the plan,
validation evidence, and final handoff. Every Workpad update must include one
`detent-status` fenced block. Detent reads blocker and human-action
declarations from that block; narrative sentences are never read as blockers.
`status` must be one of `in_progress`, `blocked`, or `complete`.

## Project CI Quality Gates

Replace these placeholders with the project's required stage categories and
the project-specific local command and CI check name that satisfy each one. Add
or remove categories to match the project.

- `<required-stage-category>`: local command `<project-command>`; CI check
  `<project-check-name>`
- `<required-stage-category>`: local command `<project-command>`; CI check
  `<project-check-name>`

Treat this list as part of the project contract. Whenever you touch CI
configuration or perform a review, verify that every declared stage exists,
runs its mapped project tool, and passes on the current pull request head. Do
not rely on Detent or `detent doctor` to infer required stages or inspect CI
configuration.

Use `in_progress` while implementation or validation is still active:

```detent-status
schema: 1
status: in_progress
blockers: []
human_action: null
```

Use `complete` only when the pull request is open, marked ready for review,
is not a draft, references the issue, validation is green, and no actionable
review comments remain. If the pull request is a draft, mark it ready and
verify the resulting state before declaring completion:

```sh
gh pr ready <number>
gh pr view <number> --json isDraft --jq '.isDraft' # must be false
```

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

For dependency blockers, use this order:

1. Create GitHub's native `blocked_by` dependency relation.

```sh
BLOCKED_NUMBER=<blocked-issue-number>
BLOCKER_NUMBER=<blocker-issue-number>
BLOCKER_ID="$(gh api repos/{owner}/{repo}/issues/$BLOCKER_NUMBER --jq '.id')"
gh api --method POST "repos/{owner}/{repo}/issues/$BLOCKED_NUMBER/dependencies/blocked_by" -F issue_id="$BLOCKER_ID"
```

2. Declare the blocker in the Workpad status block with `status: blocked`.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

3. Legacy fallback during the deprecation window: if native dependencies are
   unavailable and the project has not migrated, keep a machine-readable
   issue-body line such as `Blocked by: #123` or `Depends on: owner/repo#123`.

If meaningful out-of-scope work is discovered, file a separate tracker issue in Backlog with a best-guess `detent-agent` effort block instead of expanding the current work item.

## Required Execution Flow

Use the current Detent state as the source of truth for which section applies.

Before any rebase, capture the branch's effective diff against its merge base
or preserve the pre-rebase ref. After the rebase, compare with `git range-diff`
or an equivalent diff-stat and confirm the same files and hunks remain. If
changes are missing without explanation or conflict resolution dropped hunks,
stop before pushing and move the issue to the configured blocked or exception
state.

### For Todo

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

### For In Progress

1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including
   the `detent-status` block.
2. Continue from the current repository and tracker state.
3. If implementation is complete, run the full pre-review gate, update the
   Workpad block to `status: complete` with `blockers: []` and
   `human_action: null`, and move the issue to `Human Review` only when the
   gate passes.

### For Rework

1. Re-read all human and bot feedback.
2. Move the issue to `In Progress`.
3. Fix the requested changes.
4. Push updates to the pull request.
5. Run the full pre-review gate again.
6. Move the issue back to `Human Review` only when the gate passes.

### For Merging

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
