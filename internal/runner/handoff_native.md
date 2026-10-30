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


For every human_action, set block_kind to criteria_defect (a criterion needs correction), product_decision (a product choice), missing_authorization (permission is required), or physical_action (a person must act). Quote the exact affected acceptance criterion in criterion, or name the exact decision in human_action. State which parts of the issue you already completed in the final summary. Supply options as a list of concrete answers, each with an answer field. For criteria_defect, criterion is required and at least one option must include replacement_criterion containing a proposed criterion verifiable before merge. Example:

```yaml
block_kind: criteria_defect
criterion: Run the integration on a physical device
options:
  - answer: Verify the integration with an isolated device fixture
    replacement_criterion: The isolated device fixture passes the integration assertions before merge
```
