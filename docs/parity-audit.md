# Detent Go/Elixir Parity Audit

This checklist tracks the standing parity audit against the archived Elixir
Detent. Revisit it each milestone and link gap issues instead of expanding
audit work into feature implementation.

## Baseline

- Audit issue: digitaldrywood/detent#145
- Audit date: 2026-05-31
- Elixir dashboard reference:
  `elixir/lib/detent_elixir_web/live/dashboard_live.ex`
- Elixir terminal reference:
  `elixir/lib/detent_elixir/status_dashboard.ex`
- Go web dashboard: `internal/web/templates/dashboard.templ`
- Go terminal dashboard: `internal/tui/model.go`
- Go telemetry model: `internal/telemetry/snapshot.go`

## Checklist

| Area | Elixir parity target | Go status | Gap issue | Notes |
| --- | --- | --- | --- | --- |
| Web summary counts | Running, retry pressure, blocked, tokens, spend today, runtime | Partial | digitaldrywood/detent#148, digitaldrywood/detent#149 | Go shows running, queue, blocked, completed, budget, rate limits, and tokens. It does not yet expose rolling TPS, lifetime totals, spend-today sparkline, or degraded stats. |
| Web running sessions | Issue identity, state, session, runtime/turns, Codex update, diff, tokens | Partial | digitaldrywood/detent#147 | Go renders the core row data. Missing copy/open affordances, JSON detail links, and issue popovers from the Elixir dashboard. |
| Web retry/backoff queue | Attempt, due time, and retry error | Gap | digitaldrywood/detent#147 | Go telemetry has queued rows, and the TUI renders them, but the web dashboard only exposes the queue count. |
| Web blocked sessions | Blocked issue, state, session, blocked time, last update, error | Gap | digitaldrywood/detent#147 | Go telemetry has blocked rows, and the TUI renders them, but the web dashboard only exposes the blocked count. |
| Web recent sessions | Completed time, runtime/turns, tokens, final state, model | Gap | digitaldrywood/detent#147 | Go telemetry has completed rows, and the TUI renders them, but the web dashboard only exposes the completed count. |
| Web budget history | Spend today against cap plus seven-day sparkline | Partial | digitaldrywood/detent#149 | Go budget card shows current/projected spend and caps. Budget day history is present in telemetry but not rendered in the web template. |
| Web health and density | Live/offline badges, stats degraded indicator, comfortable/compact density | Gap | digitaldrywood/detent#149 | Go shows connector and update time. Health/degraded state and density controls are not represented. |
| Rate limits | Primary, secondary, credits, reset details | Covered | None | Go web and TUI render Codex rate-limit snapshots, including credit availability. |
| Token totals | Input, output, total tokens | Covered | None | Go web and TUI render token totals and running-session token totals. |
| Throughput | Rolling tokens per second with throttled updates | Gap | digitaldrywood/detent#148 | Go web has a derived tokens/minute card. Go TUI does not yet render the Elixir throughput line. |
| Runtime totals | Current run runtime and all-time runtime/session totals | Partial | digitaldrywood/detent#148 | Go has snapshot runtime seconds. Lifetime totals and degraded stats handling are missing from the telemetry contract. |
| TUI running rows | ID, stage, PID, age/turn, tokens, session, event | Partial | digitaldrywood/detent#150 | Go renders ID, stage, host, age/turn, tokens, session, and event. PID/process identity is not modeled separately. |
| TUI backoff queue | All queued retries sorted by due time | Covered | None | Covered by `internal/tui/status_dashboard_parity_test.go`. |
| TUI project links | Project, dashboard URL, next refresh | Gap | digitaldrywood/detent#150 | Go TUI renders generated time but not project/dashboard/refresh lines. |
| Dispatch order | Elixir candidate filtering, priorities, state caps, blocked dependencies | Covered | None | Covered by `internal/orchestrator/dispatch_parity_test.go` and related orchestrator tests. |
| Scheduler fairness | Weighted, strict priority, round-robin, fair-share project selection | Covered | None | Covered by `internal/scheduler/global_test.go` and project manager scheduler tests. |
| Merge train serialization | `Merging` state intentionally capped to one active agent | Covered | None | Documented in `README.md` and generated onboarding workflow defaults. |
| Workspace hooks | after_create, before_run, after_run, before_remove | Covered | None | Covered by `internal/workspace/workspace_test.go` and wired through project runner construction. |
| Codex app-server transcript | Elixir transcript byte-level JSON-RPC compatibility | Covered | None | Covered by `internal/codex/appserver_parity_test.go`. |
| GitHub Projects adapter | ProjectV2 state mapping, dependency parsing, status updates | Covered | None | Covered by `internal/connector/github/adapter_parity_test.go`. |
| Shadow comparison | Go-vs-Elixir dispatch observation diffs | Covered | None | Covered by `internal/shadow` comparator tests. |

## Gap Issues

- digitaldrywood/detent#147: Restore web dashboard work queues and session detail parity.
- digitaldrywood/detent#148: Add dashboard throughput and lifetime runtime parity.
- digitaldrywood/detent#149: Add budget history and dashboard health indicators.
- digitaldrywood/detent#150: Restore TUI project refresh and process identity parity.

## Milestone Audit Notes

- 2026-05-31: Initial standing checklist added. The Go rewrite already has
  parity coverage for dispatch ordering, scheduler fairness, workspace hooks,
  Codex app-server transcript handling, GitHub Projects adapter behavior, TUI
  backoff rows, rate-limit formatting, and token totals. The remaining gaps are
  dashboard presentation and telemetry fields rather than core dispatch
  behavior.
