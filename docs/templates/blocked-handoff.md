## Blocked handoff

Workpad: plan, validation, one schema-1 `detent-status`: `in_progress`, `blocked`, or `complete`. Prose is not a blocker.

Comment projects: edit current authoritative `## Codex Workpad` if permitted, else post anew; body/final cannot supersede it. Keep native/local writers.

Record gate/current-head checks; reuse verified same-head/test-input receipts without handoff-only reruns. Skips earn no credit; merge-group CI must pass.

The orchestrator is the only writer of tracker lane state. Never change lane labels or status fields.

Before coding, POST blocker `issue_id` to `dependencies/blocked_by`; keep `Depends on: owner/repo#123`. Positive `#N`/`owner/repo#N` only, no URLs/YAML `blocked_by`; instance-owned `instance:tool` clears via Workpad.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "dependency must merge"
human_action: null
```

`blocked` needs blocker, `human_action`, or `reason_code`; issue-state/tick checks never clear reason-only blockers. Credential/write failures are instance-owned. Finish independent work; keep PR. Human needs: `human_action`; resume: authorized newer Workpad, `in_progress`, `human_action: null`. Invent no dependencies/breaker acknowledgments; replies authorize only stated actions.

Success:

```detent-status
schema: 1
status: complete
{{ completion_fields }}blockers: []
human_action: null
```

Merged work needs no authorization. `fields`: `completion_kind: operational`, `completion_evidence` (acceptance), `completion_merged_pr`/`completion_merge_commit` (URL/SHA), `completion_branch`/`completion_branch_head` (tested PR-base integration ref/SHA, never workspace), `completion_ancestry: verified` after fetch + `git merge-base --is-ancestor` succeeds. Missing evidence: blocked Workpad `human_action`. Other no-PR: issue-body `detent-completion` authorization.
