# Runner Telemetry And APIs

[Back to README](../README.md#documentation)

Each enrolled runner serves a local health and telemetry API on its configured
listener; the board, issues, and Change Requests live in the Hub (see
[Hub API](hub-api.md)). The runner's state snapshot reports live counts,
running issues, retry queue, blocked work, completed sessions, token totals,
budget status, and provider rate-limit snapshots.

Host memory, IO, and CPU PSI appear in `detent state`, `/api/v1/state`, and
`/health` as `memory_pressure`, `io_pressure`, and `cpu_pressure`. Each entry
includes the current governing average, configured threshold, support or
read-error state, configured and effective pressure capacity, constraint
duration, and whether Detent is holding new dispatches. A pressure constraint
is admission backpressure, not a worker failure: existing agents continue while
new starts wait for available degraded capacity or pressure recovery.

When an agent backend reports `model_context_window`, Detent also surfaces
context pressure for running and recent sessions. Context pressure is the
session's `total_tokens` divided by the model context window. The JSON snapshot
includes `context_pressure.total_tokens`, `context_limit_tokens`,
`percent_used`, and `threshold_state`. Thresholds are `normal` below 70%, `watch` at
70%, `warning` at 85%, and `critical` at 95%. Unknown context windows omit the
derived fields instead of reporting a misleading zero.

Context pressure is a model-window signal, not a Detent stop threshold.
For Codex, `codex.turn_timeout_ms` is an inter-message liveness bound, despite
its name. Each stream message starts a new timer. Omitting the key still uses
the one-hour (`3600000` ms) default, and continuous messages can keep one turn
alive indefinitely. Lowering this value therefore detects silence sooner but
does not cap total turn duration. `codex.stall_timeout_ms` is also reset by
stream activity; when both liveness bounds are enabled, the shorter deadline
wins for each receive.

`agent.max_turns` bounds the provider turns reported during one Detent session
and is also passed to Claude Code's native turn limiter.
`agent.max_turn_duration_ms` is the total wall-clock bound for each provider
turn attempt. `agent.max_session_duration_ms` spans the full persisted Detent
session, including a failed resume attempt and its fresh fallback. The session
bound defaults to two hours (`7200000` ms); `0` disables it. The per-turn bound
defaults to `0`. When both are configured, the shorter applicable deadline
wins.

The separate `session_no_progress` timer was removed. A live local validation
queue wait remains subject to the gate lock deadline and the worker session bound.
Legacy `agent.no_progress_timeout_ms` values are accepted but ignored.

`agent.merge_worker_startup_timeout_ms` independently bounds how long a
dispatched merge runner may take to report its first startup progress. It
defaults to four minutes (`240000` ms) and is enforced by the worker context,
independent of `polling.interval_ms` and project refresh duration. Workspace
creation publishes startup progress before checkout and bootstrap begin, so
healthy workspace setup stops this timer.

`agent.merge_worker_max_duration_ms` is a separate hard wall-clock ceiling for
the full lifetime of a worker dispatched in `Merging`, starting when Detent
acquires its slot. It defaults to six hours (`21600000` ms), is not renewed by
progress, and overrides the disabled general duration defaults for merge work.
On breach, Detent cancels the owned worker, releases its slot, logs the elapsed
time and last progress marker at WARN, and parks the issue in `Blocked`.

`agent.max_session_tokens` is an absolute configured ceiling for a session.
`total_tokens` counts input, output, cache-created, and cache-read tokens,
accumulated across every turn of the session — cached context is re-counted on
each turn, so a healthy session accrues millions of tokens within minutes.
Use `max_session_tokens` as an additional token-consumption backstop; a value
near one turn's worth of context terminates every session at its ceiling.
`agent.max_session_context_multiplier` derives a ceiling from the reported
context window when that window is known. A session can show high context
pressure before either ceiling is exceeded, and a low-pressure session can
still hit a lower absolute `agent.max_session_tokens` value. Token rows also
show cache-read efficiency when cached input is reported: cached input divided
by input tokens. Use that value with context pressure to evaluate whether
thread-resume behavior is preserving useful context without repeatedly filling
the window.

`budget.billing_mode` accepts `metered` or `subscription`. Metered mode
enforces configured USD caps and the USD progress breaker. Subscription mode
keeps notional spend telemetry but never refuses dispatch or parks work based
on USD; Detent instead scales dispatch concurrency with the lowest reported
primary or secondary provider rate-window percentage. An omitted mode defaults
to `subscription`, so USD controls are inert unless metered billing is declared
explicitly.

Subscription pacing is configured globally with `global.rate_window_pacing`
and overridden per project with `agent.rate_window_pacing`. The default
`proportional` mode preserves the existing percentage scaling. `off` disables
scaling, and `floor` keeps full concurrency until remaining capacity falls
below `floor_percent`. Missing buckets, buckets without an observation
timestamp, and buckets older than `stale_after_seconds` fail open to
`agent.max_concurrent_agents`. Both `/health` and board-state responses expose
the resolved mode, bucket freshness, observed remaining percentage, and
effective permit ceiling at `dispatch.rate_window_pacing`.

`agent.no_progress_token_limit` defaults to `25000000` tokens and is enforced
in both billing modes. Detent sums persisted `total_tokens` for the issue across
attempts after its latest accepted lane or PR advancement. Reaching the limit
parks the issue even when USD is only notional subscription telemetry. Set the
value to `0` to disable the token breaker.

`agent.no_progress_spend_limit_usd` defaults to a base limit of `3`, below the
default per-issue budget backstop, and is enforced only when
`billing_mode: metered`. The effective limit scales with the session's reasoning
effort: unknown/low `1x`, medium `1.5x`, high `3x`, xhigh `6x`, and max/ultracode
`8x`. These multipliers assume one retry at the observed cost profile should fit
before the breaker fires; `detent doctor` warns when an effort tier's effective
limit is below its observed p50 per-session cost and recommends a base limit
with `1.5x` retry-cost headroom.

Duration limits stop a single overlong turn or session regardless of message
volume. The cross-session progress breakers can stop repeated or expensive work
even when each individual session stays below its duration cap. The token
breaker remains blocking on subscription fleets; the USD breaker is
metered-only.

PR creation, a new head commit, a dirty-to-clean mergeability transition, and a
failing-to-passing CI transition each reset the shared token and USD window.
Usage accumulates only while the PR fingerprint remains static. When an
effective limit trips, Detent parks the issue in `Blocked` and identifies
whether no PR evidence was produced or a linked PR remained static. The latter
points operators toward `Merging` capacity and serialization tuning; the
former recommends narrowing or splitting the task. The next worker must explain
the missing progress signal in its first Workpad update before using tools.
`detent doctor` reports both effective brakes for every project and warns when
neither is active.

`agent.failure_breaker` pauses new project dispatches when the same failure
class reaches `same_class_limit` attempts inside `window_seconds`. The default
is five matching failures in one hour, followed by a one-hour cooldown. After
the cooldown or a workflow reload, Detent permits exactly one canary attempt;
a success or different failure class closes the breaker, while the same class
starts a fresh cooldown. The state and health APIs report failed-attempt and
distinct-item counts, affected item links and parking state, a representative
operator cause, backend/provider identity, any matching capacity outage, the
absolute resume time, and the current eligible-candidate count. Canary
eligibility does not make a parked item retryable. The eligible count includes only candidates that dispatched or were
held solely by the project breaker; dependency, budget, provider-capacity, and
other dispatch gates remain separate reasons that can leave the count at zero.

Daily budget caps are scoped to the configured project. Session rows persist
the project ID; on upgrade, Detent backfills older rows first from work
attempts and then by matching each identifier's repository prefix against the
configured project registry. Any session that remains unattributed counts
toward every project's daily cap as a conservative fallback. `detent doctor`
warns while unattributed completed sessions exist for the current UTC day.

`agent.resume_orphaned_sessions` defaults to `true`. After an unclean Detent
restart, active sessions whose provider identity was journaled are preflighted
and resumed with a short continuation prompt. Missing provider state,
unsupported backends, and failed resume handshakes automatically fall back to
the full fresh continuation prompt. Set the field to `false` to retain fresh
redispatch behavior for every restart.

Completed issues persist an efficiency receipt built from session, attempt,
usage, and workflow-lane rows. Efficiency reporting covers per-merged-issue
percentiles, cache share,
first-attempt merge rate, dwell decomposition, anomalies, and a trailing-window
baseline. The default anomaly threshold is 3x the project baseline and can be
changed per workflow. OTLP lifecycle export is optional and disabled when no
endpoint is configured:

```yaml
observability:
  efficiency:
    anomaly_tokens_multiple: 3
    anomaly_sessions_multiple: 3
    anomaly_dwell_multiple: 3
  otlp:
    endpoint: http://127.0.0.1:4318
    service_name: detent
    timeout_ms: 5000
```

The exporter posts OTLP HTTP/JSON traces to `/v1/traces` with linked
`detent.dispatch`, `detent.session`, `detent.gate`, and `detent.merge` spans.
Static collector headers may be supplied with `observability.otlp.headers`.

Useful endpoints:

| Route | Purpose |
| --- | --- |
| `/health` | Server health, startup readiness, and configured dependency checks. |
| `/api/v1/openapi.yaml` | Public OpenAPI 3 catalog for the stable JSON API. HTML, HTMX, and SSE routes are excluded. |
| `/api/v1/state` | JSON telemetry snapshot. `projection=cli` bounds collections and response bytes with explicit truncation metadata; `fields=update,counts` retains the updater projection. |
| `/api/v1/timeseries?window=10m&bucket=1m` | Fleet chart samples for running agents, tokens/sec, and completions. |
| `/api/v1/operator-tools/<name>` | Invoke one shared read-only operator tool with a JSON object via `POST`; requires read scope and rejects mutation tools. |
| `/api/v1/projects/<id>/state` | Project-scoped JSON telemetry snapshot; supports the same `projection=cli` after project scoping. |
| `/api/v1/projects/<id>/timeseries?window=10m&bucket=1m` | Project chart samples for running agents, token spend, and board flow. |
| `/api/v1/projects/<id>/issues/explanation?reference=<issue>` | Versioned JSON explanation of an issue's current lane, runtime state, evidence, and degraded sources. |
| `/api/v1/refresh` | Request an orchestrator refresh with `POST`. |
| `/api/v1/<issue>` | JSON detail for a known board, pipeline, running, retrying, or blocked issue. |

State responses calculate `snapshot_age_seconds` when the request is served,
so counts remain attributable to their producer timestamp even if snapshot
publication stops. Refresh objects include `next_refresh_overdue`; an explicit
`ready` status becomes `behind` once `next_refresh_at` is in the past. `behind`
is a pacing state and keeps successful tracker data operational. The refresh
object reports `behind_by_seconds`, each project reports
`last_duration_seconds`, and the fleet reports `observed_sweep_seconds`.
`stale_after_seconds` expands with the observed multi-project sweep and includes
headroom. A refresh becomes `degraded` only after that window is exceeded or a
refresh source reaches its failure threshold.
`/health` returns HTTP `503` with `status: "not_ready"`, `ready: false`, and a
`lifecycle` of `starting` until running-mode startup succeeds. A fatal startup
changes `lifecycle` to `failed` before serving stops. Other API routes
remain available during startup for diagnostics. After startup,
`ready: true` and `lifecycle: "ready"` distinguish process readiness from
project degradation and other operational health details.

`/health` also reports `snapshot_generated_at`, `snapshot_age_seconds`, the
current refresh object, per-project `tick_liveness`, and
`orphaned_agent_processes` with process and session counts, total RSS, age, and
recorded session identity. Orphaned agent processes make health report
`needs_attention`; `detent doctor` lists them and recommends
`detent fix worker-processes --yes` for a confirmed reap. A tick loop that misses
two effective polling intervals reports `needs_attention`, its last tick,
overdue next refresh, missed interval count, and `frozen_at` timestamp. A loop
that continues ticking while tracker refreshes fail reports separate
`refresh_failures` entries with the project, reason, threshold, streak, and last
error. A failed first refresh is reported immediately because dispatch has
never observed the project.

Project state scopes workflow metrics from the fleet snapshot enrichment cache
instead of rerunning historical database reports for each request.

The wildcard issue route accepts an issue ID, canonical identifier, issue URL,
bare number, or `#number`. Add `?project=<id>` when a number or other reference
exists in more than one project; an unscoped collision returns
`ambiguous_issue_reference`. The response reports the board `lane` separately
from runtime `activity` (`idle`, `running`, `retrying`, or `blocked`). Board data
takes precedence over pipeline and runtime copies for lane and identity, while
runtime activity uses running, retrying, then blocked precedence. Completed and
tracker-drift-only items are not part of this route.

### API Authentication

Configure a top-level `api_token` in `global.yaml`, or set
`DETENT_API_TOKEN` to override it at runtime. Use a high-entropy value; the
recommended shape is a `detent_` prefix followed by a random secret. Mutating
API routes require `Authorization: Bearer <token>` or `X-API-Key: <token>`.
When a token is configured, read-only `GET /api/v1/*` routes require it too.
`GET /health` stays unauthenticated.

If Detent binds a non-loopback host such as `0.0.0.0` without an `api_token`,
API routes fail closed and mutating routes return `403` until a token is
configured. With no token on loopback, read-only API routes remain open for
local development.

For a non-loopback bind that still needs tokenless same-host reads, opt in with
`trust_loopback_peer_read: true` in `global.yaml`. Detent then grants a
read-only credential to `GET` requests whose raw TCP peer address is loopback,
even when `api_token` is configured. A supplied invalid, expired, or revoked
token still fails authentication. `X-Forwarded-For`, `Forwarded`,
`X-Real-IP`, and other forwarded-client metadata never affect this decision.
The setting hot-reloads.

Do not enable `trust_loopback_peer_read` behind a reverse proxy on the same
host. Every remote request relayed by that proxy appears to Detent to have a
loopback direct peer and would receive read access.

## Work attempt recovery

`POST /api/v1/projects/{project}/work-attempts/{attempt}/recovery` accepts
`retry_fresh` or `retry_resume` for the latest failed attempt. Recovery validates
the current tracker issue and model selection, records retry intent in the
workflow journal, reconciles the matching corrected configuration failure, and
wakes the scheduler. Duplicate requests reuse that intent; a newer attempt
supersedes it. The intent survives restart without acknowledging a later park.

The response distinguishes `queued`, `blocked`, and `running`. `queued` means
the durable intent awaits scheduler admission. `blockers` lists the current
holds observed during reconciliation, and `next_action` describes the recheck.
`current_attempt_id` identifies the running or superseding attempt when known.
Inspect the receipt again to follow recovery through worker dispatch.

Park acknowledgement records which park sequence the operator reviewed. It
does not establish eligibility or worker start. Recovery preserves independent
dependencies, approvals, ownership, budget limits, and service outages. Invalid
issue configuration is an actionable issue hold, rather than project-outage
evidence.
