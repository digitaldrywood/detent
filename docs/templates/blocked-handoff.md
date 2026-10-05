## Blocked handoff

Workpad: plan, validation, one schema-1 `detent-status`: `in_progress`, `blocked`, or `complete`. Prose is not a blocker.

Comment projects: edit current authoritative `## Codex Workpad` if permitted, else post anew; body/final cannot supersede it. Keep native/local writers.

Record gate/current-head checks; reuse verified same-head/test-input receipts without handoff-only reruns. Skips earn no test credit; merge-group CI must pass.

The orchestrator is the only writer of tracker lane state. Never change lane labels or status fields.

POST real blocker `issue_id` to `dependencies/blocked_by` before coding; keep `Depends on: owner/repo#123`. Refs: positive `#N`/`owner/repo#N`, no URLs/YAML `blocked_by`; instance-owned `instance:tool` clears via Workpad.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "dependency must merge"
human_action: null
```

`blocked` needs blocker, `human_action`, or `reason_code`; default issue-state/tick checks never clear reason-only blockers. Credential/write failures are instance-owned. Finish independent work; keep PR. Human needs: blocked `human_action`; resume needs authorized newer Workpad evidence, `in_progress`, `human_action: null`. Invent no dependencies/breaker acknowledgments; replies authorize only stated actions.

Success:

```detent-status
schema: 1
status: complete
{{ completion_fields }}blockers: []
human_action: null
```

Already-merged work needs no authorization. Operational `fields`: `completion_kind: operational`, `completion_evidence` (acceptance), `completion_merged_pr`/`completion_merge_commit` (URL/SHA), `completion_branch`/`completion_branch_head` (tested PR-base integration ref/SHA, never workspace), `completion_ancestry: verified` after fetch + successful `git merge-base --is-ancestor`. Missing evidence needs a blocked Workpad `human_action`. Other no-PR work: issue-body `detent-completion` authorization.
