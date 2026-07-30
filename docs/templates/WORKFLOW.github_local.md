You are working on {{ issue.identifier }}: {{ issue.title }}.
Current Detent status: {{ issue.state }}.

This workflow uses `tracker.kind: github_local`. GitHub issues and pull
requests are read-only inputs. Detent status, claim fields, audit-trail
comments, and close decisions stay in the local SQLite database configured by
`tracker.local_sqlite.path`; do not add `tracker.github_status_source` to this
file.

Import issues explicitly before dispatching:

```sh
detent github-local import <local-detent-project-id> <issue-number>[,<issue-number>...] --state Todo
detent doctor --project <local-detent-project-id> --port 0
```

Follow repository instructions, keep changes scoped to the issue, and keep the
local `## Codex Workpad` record updated through Detent events. Every Workpad
update must include one `detent-status` fenced block. Detent reads blocker and
human-action declarations from that block; narrative sentences are never read as
blockers. GitHub issue comments, labels, Projects, issue fields, and issue close
state must remain untouched. Pull request comments and merges created by Detent
are allowed for Detent-owned PR lifecycle work. `status` must be one of
`in_progress`, `blocked`, or `complete`.

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

For dependency blockers in this local-status workflow, declare the blocker in
the Workpad status block with `status: blocked`:

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

If a human explicitly authorizes upstream GitHub dependency metadata writes
outside this read-only local-status workflow, prefer GitHub's native
`blocked_by` dependency relation:

```sh
BLOCKED_NUMBER=<blocked-issue-number>
BLOCKER_NUMBER=<blocker-issue-number>
BLOCKER_ID="$(gh api repos/{owner}/{repo}/issues/$BLOCKER_NUMBER --jq '.id')"
gh api --method POST "repos/{owner}/{repo}/issues/$BLOCKED_NUMBER/dependencies/blocked_by" -F issue_id="$BLOCKER_ID"
```

Legacy fallback during the deprecation window: if a human explicitly authorizes
upstream GitHub metadata writes, native dependencies are unavailable, and the
project has not migrated, keep a machine-readable issue-body line such as
`Blocked by: #123` or `Depends on: owner/repo#123`.

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

### For In Progress

Continue implementation from the current repository state. If implementation is
complete, run the validation gate, update the Workpad block to
`status: complete` with `blockers: []` and `human_action: null`, push the PR
branch, and move the issue to `Human Review`.

### For Rework

Read all review feedback, fix the requested changes, rerun validation, push the
branch, and move the issue back to `Human Review` only when the gate passes.

### For Merging

Rebase onto current `origin/main`, rerun the configured validation gate, push,
watch current-head CI, merge with the configured PR workflow once green, and
move the issue to `Done`. If an external blocker remains, keep the issue in
`Merging` and record the exact blocker in the `detent-status` block.
