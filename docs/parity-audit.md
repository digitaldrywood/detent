# Detent Go/Elixir Parity Audit

This checklist tracks the standing parity audit against the archived Elixir
Detent. Revisit it each milestone and link gap issues instead of expanding
audit work into feature implementation.

## Baseline

- Audit issue: digitaldrywood/detent#145
- Audit date: 2026-06-19
- Elixir dashboard reference:
  `elixir/lib/detent_elixir_web/live/dashboard_live.ex`
- Elixir terminal reference:
  `elixir/lib/detent_elixir/status_dashboard.ex`
- Go web dashboard: `internal/web/templates/dashboard.templ`
- Go web project pages: overview, Kanban, runs, configuration, and diagnostics
- Go terminal dashboard: `internal/tui/model.go`
- Go telemetry model: `internal/telemetry/snapshot.go`

## Checklist

| Area | Elixir parity target | Go status | Gap issue | Notes |
| --- | --- | --- | --- | --- |
| Web summary counts | Running, retry pressure, blocked, tokens, spend today, runtime | Covered | Closed: digitaldrywood/detent#148, digitaldrywood/detent#149 | Overview and diagnostics render running, queue, blocked, completed, budget, rate limits, rolling throughput, lifetime totals, and degraded stats. |
| Web running sessions | Issue identity, state, session, runtime/turns, Codex update, diff, tokens | Covered | Closed: digitaldrywood/detent#147 | Dedicated project runs page renders running-session rows, issue previews, links, JSON/detail affordances, session, diff, runtime, turns, and tokens. |
| Web retry/backoff queue | Attempt, due time, and retry error | Covered | Closed: digitaldrywood/detent#147 | Project runs page renders retry rows with due time, attempt/error context, and issue detail links. |
| Web blocked sessions | Blocked issue, state, session, blocked time, last update, error | Covered | Closed: digitaldrywood/detent#147 | Project runs page renders blocked rows with state, session, blocked time, last update, error, and dependency context. |
| Web recent sessions | Completed time, runtime/turns, tokens, final state, model | Covered | Closed: digitaldrywood/detent#147 | Project runs page renders recent completed sessions with final state, model, runtime, turns, and tokens. |
| Web budget history | Spend today against cap plus seven-day sparkline | Covered | Closed: digitaldrywood/detent#149 | Budget card renders current/projected spend, caps, burn-down projection, and daily history bars from telemetry. |
| Web health and density | Live/offline badges, stats degraded indicator, comfortable/compact density | Covered | Closed: digitaldrywood/detent#149 | Dashboard and diagnostics render connector health, update time, degraded stats, and compact responsive cards. |
| Rate limits | Primary, secondary, credits, reset details | Covered | None | Go web and TUI render Codex rate-limit snapshots, including credit availability. |
| Token totals | Input, output, total tokens | Covered | None | Go web and TUI render token totals and running-session token totals. |
| Throughput | Rolling tokens per second with throttled updates | Covered | Closed: digitaldrywood/detent#148 | Go web and TUI render token throughput from the telemetry contract. |
| Runtime totals | Current run runtime and all-time runtime/session totals | Covered | Closed: digitaldrywood/detent#148 | Telemetry includes lifetime totals and degraded reasons; web renders lifetime token, runtime, session, and run totals. |
| TUI running rows | ID, stage, PID, age/turn, tokens, session, event | Covered | Closed: digitaldrywood/detent#150 | Go renders ID, stage, process identity/PID, age/turn, tokens, session, and event. |
| TUI backoff queue | All queued retries sorted by due time | Covered | None | Covered by `internal/tui/status_dashboard_parity_test.go`. |
| TUI project links | Project, dashboard URL, next refresh | Covered | Closed: digitaldrywood/detent#150 | Go TUI renders project URL, dashboard URL, generated time, and next refresh. |
| Dispatch order | Elixir candidate filtering, priorities, state caps, blocked dependencies | Covered | None | Covered by `internal/orchestrator/dispatch_parity_test.go` and related orchestrator tests. |
| Scheduler fairness | Weighted, strict priority, round-robin, fair-share project selection | Covered | None | Covered by `internal/scheduler/global_test.go` and project manager scheduler tests. |
| Merge train serialization | `Merging` state intentionally capped to one active agent | Covered | None | Documented in `README.md` and generated onboarding workflow defaults. |
| Workspace hooks | after_create, before_run, after_run, before_remove | Covered | None | Covered by `internal/workspace/workspace_test.go` and wired through project runner construction. |
| Codex app-server transcript | Elixir transcript byte-level JSON-RPC compatibility | Covered | None | Covered by `internal/codex/appserver_parity_test.go`. |
| GitHub Projects adapter | ProjectV2 state mapping, dependency parsing, status updates | Covered | None | Covered by `internal/connector/github/adapter_parity_test.go`. |
| Shadow comparison | Go-vs-Elixir dispatch observation diffs | Covered | None | Covered by `internal/shadow` comparator tests. |

## Restored Gap Issues

- digitaldrywood/detent#147: Restore web dashboard work queues and session detail parity. Closed.
- digitaldrywood/detent#148: Add dashboard throughput and lifetime runtime parity. Closed.
- digitaldrywood/detent#149: Add budget history and dashboard health indicators. Closed.
- digitaldrywood/detent#150: Restore TUI project refresh and process identity parity. Closed.

## Milestone Audit Notes

- 2026-05-31: Initial standing checklist added. The Go rewrite already has
  parity coverage for dispatch ordering, scheduler fairness, workspace hooks,
  Codex app-server transcript handling, GitHub Projects adapter behavior, TUI
  backoff rows, rate-limit formatting, and token totals. The remaining gaps are
  dashboard presentation and telemetry fields rather than core dispatch
  behavior.
- 2026-06-19: Web dashboard parity gaps #147, #148, and #149 are closed through
  the project overview/runs/diagnostics split, budget history rendering,
  throughput/lifetime telemetry, and degraded stats. TUI parity gap #150 is
  closed through project/dashboard/refresh headers and process identity rows.
