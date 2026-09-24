## Blocked handoff

One `## Codex Workpad`: plan, validation, one `detent-status` fence (`schema: 1`; `in_progress`, `blocked`, or `complete`). Prose is not a blocker.

PR handoff: gate and current-head checks. Skips are not passes; expected merge-group skips allow handoff. Merge-group CI must pass.

The orchestrator is the only writer of tracker lane state. Never change lane labels or status fields.

POST real blocker `issue_id` to `dependencies/blocked_by` before coding; retain `Depends on: owner/repo#123`. Refs: positive `#N` or `owner/repo#N`, not URLs. Symbolic refs (`instance:tool`) are instance-owned; clear via Workpad. No YAML `blocked_by`.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

Defaults: issue-state/tick checks; orchestrator owns. `blocked` needs blocker, `human_action`, or `reason_code`; reason-only blockers never auto-clear.

Credential/write-policy failures are instance errors. Finish independent work. For real human needs, set `status: blocked` with concrete `human_action`; Detent shows "Needs you" in Blocked. Keep the PR. Never invent dependencies or acknowledge breaker parks. Replies authorize only what they say.

Success:

```detent-status
schema: 1
status: complete
{{ completion_fields }}blockers: []
human_action: null
```

Already-merged work needs no authorization. In `fields`, set `completion_kind: operational`, `completion_evidence` (acceptance results), `completion_merged_pr` (URL), `completion_merge_commit` (SHA), `completion_branch` (tracked ref), `completion_branch_head` (SHA), and `completion_ancestry: verified` after fetch and successful `git merge-base --is-ancestor`. Missing evidence needs a blocked Workpad `human_action`. Other no-PR work needs issue-body `detent-completion` authorization.
