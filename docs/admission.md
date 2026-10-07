# Admission criteria

[Back to README](../README.md#documentation)

Admission criteria and the issue effort rubric are project-owned text. The
lists below are examples; Detent has no built-in rubric.

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
