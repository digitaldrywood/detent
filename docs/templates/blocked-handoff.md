## Blocked handoff

One `## Codex Workpad`: plan, validation, one `detent-status` fence (`schema: 1`; `in_progress`, `blocked`, or `complete`). Prose is not a blocker.

PR handoff: record gate and current-head checks. Expected skips allow handoff, not test credit; merge-group CI must pass.

The orchestrator is the only writer of tracker lane state. Never change lane labels or status fields.

POST real blocker `issue_id` to `dependencies/blocked_by` before coding; retain `Depends on: owner/repo#123`. Refs: positive `#N` or `owner/repo#N`; no URLs or YAML `blocked_by`. Symbolic `instance:tool` refs are instance-owned; clear via Workpad.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

Defaults: issue-state/tick checks. `blocked` needs blocker, `human_action`, or `reason_code`; reason-only blockers never auto-clear.

Credential/write failures belong to the instance. Finish independent work. Human needs use blocked Workpad `human_action` ("Needs you"). An authorized member posts a newer Workpad with evidence, `status: in_progress`, and `human_action: null`; Detent resumes. Keep the PR. No invented dependencies or breaker acknowledgments. Replies authorize only stated actions.

Success:

```detent-status
schema: 1
status: complete
{{ completion_fields }}blockers: []
human_action: null
```

Already-merged work needs no authorization. In `fields`, set `completion_kind: operational`, `completion_evidence` (acceptance results), `completion_merged_pr` (URL), `completion_merge_commit` (SHA), `completion_branch` (tracked ref), `completion_branch_head` (SHA), and `completion_ancestry: verified` after fetch and successful `git merge-base --is-ancestor`. Missing evidence needs a blocked Workpad `human_action`. Other no-PR work needs issue-body `detent-completion` authorization.
