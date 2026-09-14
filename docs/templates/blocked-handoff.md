## Blocked handoff

Maintain one `## Codex Workpad` comment with plan, validation, and one `detent-status` fence per update. Use `schema: 1`; `status` must be exactly one of `in_progress`, `blocked`, or `complete`; no other value is valid. Narrative Workpad sentences are never read as blockers.

The orchestrator is the only writer of tracker lane state. Never change lane labels or status fields, even if WORKFLOW says otherwise.

Resolve dependencies before coding. Create GitHub's native `dependencies/blocked_by` relation (POST the blocker's REST `issue_id`); retain an issue-body `Depends on: owner/repo#123` or `Blocked by: #123`. YAML `ref` accepts only `#N` or `owner/repo#N`, positive N, not URLs. `blocked_by` is not a YAML key.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

Refs default to issue-state checks, orchestrator ownership, and tick rechecks. Reason-only blockers never auto-clear. `blocked` needs a blocker, `human_action`, or `reason_code`. See docs/structured-workpad-signaling-migration.md for optional predicates and recovery codes.

Report credentials/write-policy failures as instance errors, never dependencies or manual-PR requests. Finish independent work before `ask_human_question`; keep `in_progress` and the PR. Never synthesize dependencies or acknowledge breaker parks. Replies authorize only what they say; clarify ambiguity. Report a missing question tool.

For ongoing work use `in_progress`; on success:

```detent-status
schema: 1
status: complete
{{ completion_fields }}blockers: []
human_action: null
```

No-PR completion requires pre-dispatch issue-body `detent-completion` authorization (`schema: 1`, `completion_kind: operational`). Add `completion_kind: operational` and concrete `completion_evidence` under `fields`, retaining attempt identity. Otherwise the PR gate applies.
