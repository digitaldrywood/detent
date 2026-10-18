# Admission criteria

[Back to README](../README.md#documentation)

Admission criteria and the issue effort rubric are project-owned text. The
lists below are examples; Detent has no built-in rubric.

The shared project workflow can define required issue sections:

```markdown
## Issue Contract

- Acceptance criteria
- Must not break
- How we know it worked
```

These three sections are the default without configured contract text. Required
sections must contain text; headings inside code fences do not count. Admission
and Todo dispatch use one evaluator. A failure uses
`agent.stop_run.target_state` (Blocked by default) with a structured
`human_action` naming missing sections and drafting a fix from the issue and
project instructions. This replaces the old completion-heading recognition.

Native creation and body edits retain human confirmation of section contents.
Machine actors and machine-origin stamps require a subsequent human body save.
Changing criteria invalidates their confirmation; effort and origin metadata
do not. A satisfying human edit clears the contract action, and existing
recorded-blocker recovery returns the issue to its recorded executable lane.
The orchestrator owns lane changes.

The native upgrade exempts issues already outside Backlog. New issues and
existing Backlog work are checked. The local runtime persists the rollout time
and checks new work or work entering Todo afterward.

- **Alignment** — Does this serve a stated current priority? Do not propose a
  candidate that maps to no stated priority.
- **Readiness** — Is the problem actionable, with the acceptance conditions
  needed by the agent that will implement it?
- **Size** — Is the work bounded enough for one agent to complete?

## Issue effort selection

- `medium` — Small, mechanical, and tightly specified.
- `high` — Standard feature or fix with ambiguity or a cross-cutting surface.
- `xhigh` — Tricky state, concurrency, restart, or recovery semantics.

An issue body can carry a `detent-agent` block that sets `effort` for that
issue. Each stage clamps it to its configured ceiling; see
[session limits by complexity level](config.md#session-limits-by-complexity-level).
`detent doctor` warns when a project's `AGENTS.md` and `CLAUDE.md` contain no
`detent-agent` guidance.

The `backlog_admission` proposal pass reads candidates through tracker
selectors that `tracker.kind: hub_native` does not provide, so native projects
cannot enable it.
