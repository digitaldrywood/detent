# Runner Telemetry And APIs

[Back to README](../README.md#documentation)

Each enrolled runner serves a local health endpoint on its configured listener.
The board, issues, Changes, telemetry and reports live in the Hub (see
[Hub API](hub-api.md)).

Host memory, IO, and CPU PSI constrain admission. A pressure constraint
is admission backpressure, not a worker failure: existing agents continue while
new starts wait for available degraded capacity or pressure recovery.

When an agent backend reports `model_context_window`, Detent also surfaces
context pressure for running and recent sessions. Context pressure is the
session's `total_tokens` divided by the model context window. Thresholds are `normal` below 70%, `watch` at
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
time and last progress marker at WARN, records the attempt as timed out, and
hands the issue to the machine retry owner. The retry returns the issue to a
dispatchable lane with capped, jittered backoff; a machine failure never moves
an issue to `Blocked`.

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
enforces configured USD budget caps. Subscription mode keeps notional spend
telemetry but never refuses dispatch based on USD; Detent instead scales dispatch concurrency with the lowest reported
primary or secondary provider rate-window percentage. An omitted mode defaults
to `subscription`, so USD controls are inert unless metered billing is declared
explicitly.

Subscription pacing is configured globally with `global.rate_window_pacing`
and overridden per project with `agent.rate_window_pacing`. The default
`proportional` mode preserves the existing percentage scaling. `off` disables
scaling, and `floor` keeps full concurrency until remaining capacity falls
below `floor_percent`. Missing buckets, buckets without an observation
timestamp, and buckets older than `stale_after_seconds` fail open to
`agent.max_concurrent_agents`.

Duration limits stop a single overlong turn or session regardless of message
volume. Per-issue progress, token-window, spend-window and lifetime brakes are
retired: `agent.no_progress_token_limit`, `agent.no_progress_spend_limit_usd`,
`agent.no_progress_timeout_ms`, the `agent.lifetime_*` keys, and
`agent.auto_promote.rework_limit` / `no_progress_limit` no longer have any
effect. Every machine failure, including a session or merge-worker ceiling, is
retried by one retry owner with growing attempts and capped, jittered backoff
in a dispatchable lane. Spending limits belong to the budget policy.

Repeated failures are evaluated by the AI progress check using recorded attempt
history. Shared system defects use the existing deduplicated filing owner;
instance failures retain their existing capped, jittered retry backoff.

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

Local endpoints:

| Route | Purpose |
| --- | --- |
| `/health` | Process health, build version and commit, and the runner's `PATH`. Unauthenticated. |
| `/api/v1/openapi.yaml` | OpenAPI 3 document. Unauthenticated. |
| `/static/*` | Fingerprinted static assets. |
