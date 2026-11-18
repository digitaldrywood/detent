# Admission criteria

[Back to README](../README.md#documentation)

Admission criteria and any issue effort rubric are project-owned text in the project's shared workflow. Detent has no built-in rubric; it only enforces the required issue sections described here.

## Issue contract

A project's shared workflow can name the required issue sections under an `## Issue Contract` heading, one per list item:

```markdown
## Issue Contract

- Acceptance criteria
- Must not break
- How we know it worked
```

These three sections are the default when no contract text is configured. Section names must be nonempty and unique. A required section must contain text in the issue body; headings inside code fences do not count. Admission and `Todo` dispatch use the same evaluator.

When an issue in `Todo` fails the contract, Detent does not dispatch it. It moves the issue to `agent.stop_run.target_state` (`Blocked` by default) with a structured `human_action` that names the missing sections and drafts a fix from the issue and project instructions. This is a human question, so the item stays in `Blocked` until a person answers it.

Section contents need human confirmation. Issues created by a human through native creation are confirmed at creation. Issues from machine actors, issues carrying a machine-origin stamp and `[bot]` authors need a later human save of the issue body. Changing a section's text invalidates its confirmation; effort and origin metadata do not. A satisfying human edit clears the contract action, and recorded-blocker recovery returns the issue to its recorded executable lane. The orchestrator owns lane changes.

If the contract text contains the line `Agent-authored criteria do not require human confirmation.`, Detent skips the confirmation check and holds an issue with missing sections out of dispatch without moving it.

Native `Triage` items and items marked exempt are not checked. The native upgrade exempts issues already outside `Backlog`; new issues and existing `Backlog` work are checked. The local runtime persists the rollout time and checks new work or work entering `Todo` afterward.

## Issue effort selection

An issue body can carry a `detent-agent` block that sets `effort` for that issue:

```detent-agent
schema: 1
effort: high
```

Each stage clamps it to its configured ceiling; see [session limits by complexity level](config.md#session-limits-by-complexity-level). `detent doctor` reports whether the project's `WORKFLOW.md` contains a `detent-agent` effort rubric.

## Backlog admission

The `backlog_admission` proposal pass reads candidates through tracker selectors that `tracker.kind: hub_native` does not provide, so native projects cannot enable it.
