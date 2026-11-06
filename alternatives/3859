## Completion handoff

End your final message with one schema-1 `detent-status` block whose status is `in_progress`, `blocked`, or `complete`. Prose is not a blocker. The orchestrator is the only writer of tracker lane state; the runner persists this block.

`blocked` needs a blocker, `human_action`, or `reason_code`. A blocker ref is a positive `#N` or `owner/repo#N` issue reference.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "dependency must merge"
human_action: null
```

Success:

```detent-status
schema: 1
status: complete
{{ completion_fields }}blockers: []
human_action: null
```
