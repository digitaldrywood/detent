# Diagnosis from recorded history

[Back to README](../README.md#documentation)

Before concluding that a mechanism is or is not the cause, query the recorded
history for that mechanism's signature.

Throughput, concurrency, regression, and "why is this slow" questions are
answered from `work_attempts` history, never from `/api/v1/state`. The state
endpoint is a point-in-time snapshot. It can answer what Detent sees now, but
it cannot establish a trend, a sustained ceiling, or what happened before the
sample.

The dashboard's Hourly concurrency card is the first stop for throughput. It
uses a rolling 24-hour interval sweep of `work_attempts` and shows fleet-wide
and per-project median, p90, and maximum concurrency. Use the queries below
when the dashboard needs corroboration or a different window.

## Authority by question

| Question | Authoritative surface | Common trap |
|---|---|---|
| Did throughput or concurrency change? | `work_attempts` intervals, summarized over the relevant window | One `/api/v1/state` response |
| Is a capacity or rate-window mechanism binding? | Its recorded `scheduler_decisions.wait_reason` signature, correlated with `work_attempts` concurrency | Inferring a cause from a low current count or a null live bucket |
| How often did one issue dispatch? | The issue's `work_attempts` history and per-issue timeline | Board or list endpoints, which collapse repeated attempts |
| Why is an issue in this lane? | `workflow_phase_events`, or `/api/v1/workflow/timeline?project_id=<project>&identifier=<identifier>` | The issue's current lane alone |
| Which Detent build served the page? | The page's `data-detent-served-version` attribute | `detent version`, which identifies the CLI binary invoked from the shell |
| What is true right now? | `/api/v1/state` | Treating that snapshot as historical evidence |
| What does an omitted setting mean? | The loaded configuration and its code default | Treating absence as an unset runtime value |

Read the served build directly from the live page:

```sh
curl -fsS http://127.0.0.1:4000/ | rg -o 'data-detent-served-version="[^"]+"' -m 1
```

## Codex startup stalls

A `backend_startup_timeout` identifies a missed handshake deadline, not a
provider outage. The `codex app-server startup failed` log event contains
payload-free `startup` evidence even for admission runs. Issue attempts also
persist it in `work_attempts.worker_metadata_json.backend_startup`.

Compare `before_cleanup` with `process` (after cleanup). An absent
`before_cleanup` means the older runtime or transport did not capture it;
do not interpret absent counters as observed zeroes. Counters are cumulative
for that app-server process, including initialization and optional account
checks. They contain no methods, arguments, credentials, prompts, or stderr
contents.

| Observation | Interpretation and next check |
|---|---|
| `ready: false` | Initialization did not complete. Check executable/version, spawn errors, and local process pressure before investigating thread configuration. |
| `ready: true`, sent messages advance, received messages do not | Bytes were written to the provider pipe, but no additional complete RPC frame was decoded. This does not prove the provider consumed the request. Capture a bounded process sample to distinguish local blocking from remote I/O. |
| `receive_queue_depth` grows or `read_failed: true` | Investigate RPC decoding or consumer backpressure. A nonempty queue alone does not establish deadlock. |
| `stderr_bytes` grows | The provider emitted diagnostics. Inspect them locally under the operator's privacy rules; the startup event intentionally records only the count. |
| `before_cleanup.exit_observed: true` | The process exited before Detent began closing the transport. Check its exit status and launch environment. |
| Exit observed only after `termination_requested: true` | Detent requested termination during cleanup. A resulting `signal: killed` is not evidence of an earlier crash or OOM. |
| `exit_observed: true`, `cleanup_complete: false` | Process exit was observed, but the transport wait/drainers have not finished. Check inherited pipes and descendants. `cleanup_complete` records transport completion, not successful termination of every descendant. |

Response waits use one `read_timeout_ms` deadline across notifications and
server requests. Streaming turns retain their separate activity/stall rules.
A close timeout is retained in the error chain, but cannot turn an explicit
startup rejection or EOF into a startup timeout for capacity classification.
Retries retain workspace state and the existing startup failure breaker.
Admission startup failures retain their error in failed-run records instead
of becoming generic capacity deferrals. Quota and provider-overload errors
continue to defer admission without marking model output malformed.

For a recurrence, capture only the affected worker, without restarting or
signaling the live Detent service:

1. Record the attempt/session IDs, startup event, executable version, and
   failed stage. Correlate the PID with `codex_sessions.worker_pid` and
   `worker_started_at` and verify its executable and start time before sampling;
   a stale PID may belong to a different process.
2. During the stall, take a one-second macOS `sample <worker-pid> 1 1 -file
   "$TMPDIR/codex-startup.sample"` with an outer five-second command deadline.
   Record `ps -p <worker-pid> -o pid,ppid,lstart,state,%cpu,rss` alongside
   host memory/CPU pressure. Keep the raw sample private; report only relevant
   stack symbols and aggregate metrics. Existing worker concurrency and free
   disk space cannot exclude memory pressure or per-process blocking.
3. Reproduce `initialize`, `initialized`, and `thread/start` in an isolated
   app-server process with an absolute temporary workspace, no prompt or
   `turn/start`, a fixed request deadline, and guaranteed process-group cleanup.
   Match the installed protocol schema and runtime model/config. Compare a
   minimal Codex home with the configured plugins, hooks, and skills restored
   one category at a time. Keep credentials local and never print RPC payloads.
4. For launchd-only failures, compare an isolated temporary launch agent with
   a direct process. A matching TCC denial or blocking filesystem stack supports
   fixing access to that particular skill/config path; a successful direct
   launch alone does not. Do not mutate operator-owned links during diagnosis.
5. Apply a remedy only to the established cause: correct a rejected startup
   setting; remove or repair the isolated failing plugin/hook; repair the proven
   launchd access restriction; reduce spawn pressure when correlated resource
   evidence supports it; or use the provider's recovery guidance when remote
   response/connection evidence establishes an outage. Preserve existing work
   and let the existing instance breaker limit repeated pre-turn failures. Do not increase the
   timeout or restart the service to substitute for diagnosis.

### September 9, 2026 incident (#2376)

Read-only recorded history shows attempts 4959, 4960, and 4961 initialized
within 533–791 ms, then spent 5000–5001 ms waiting for `thread/start`. Attempt
4964 initialized within 338 ms and still failed after 30000 ms. All four
recorded one concurrent startup, with 8–10 active workers, and exited with
`signal: killed` after a further close deadline. Sessions 5844–5846 and 5849
were subsequently recorded as already exited by worker cleanup. Admission
session 5848 failed during the same interval and was also reaped.
Its admission run 9529 (00:45:00–00:46:10 UTC) instead recorded `deferred`,
`agent_backend_capacity`, and no error text. That loss of startup failure
attribution is reproduced by the admission regression and corrected here.

Attempt 4966 began at 00:48:20 UTC and obtained a provider identity around
00:49, before the unsuccessful timeout trial was reverted around 00:50.
The unpushed workspace commit was preserved across the failed retries.
The incident therefore establishes intermittent post-initialization silence
and bounded termination, not a cause or a timeout-setting remedy. The recorded
history lacks pre-cleanup I/O counters and process samples, so it cannot
distinguish provider deadlock, configuration/plugin I/O, resource pressure, or
external service failure retroactively. The notification deadline renewal and
cleanup-error misclassification regressions reproduce independently; neither
is claimed as the cause of these four silent waits.

## Ready-to-paste SQLite queries

These queries default to a rolling 24-hour window. Replace `'-24 hours'` or
add a `project_id` predicate when investigating a narrower incident.

### Hourly maximum-concurrency interval sweep

This event sweep counts overlapping attempt intervals without relying on a
single observation. Active attempts end at their recorded lease or heartbeat
when no completion exists, so an old incomplete row does not extend forever.

```sql
WITH RECURSIVE
params AS (
  SELECT unixepoch(strftime('%Y-%m-%dT%H:00:00Z', 'now'), '-24 hours') AS from_at,
         unixepoch(strftime('%Y-%m-%dT%H:00:00Z', 'now')) AS to_at
),
hours(hour_start) AS (
  SELECT from_at FROM params
  UNION ALL
  SELECT hour_start + 3600
  FROM hours, params
  WHERE hour_start + 3600 < to_at
),
project_hours AS (
  SELECT DISTINCT attempts.project_id, hours.hour_start,
         MIN(hours.hour_start + 3600, params.to_at) AS hour_end
  FROM work_attempts AS attempts
  CROSS JOIN hours
  CROSS JOIN params
  WHERE unixepoch(attempts.started_at) < MIN(hours.hour_start + 3600, params.to_at)
    AND unixepoch(COALESCE(attempts.completed_at, attempts.lease_expires_at,
                          attempts.heartbeat_at, attempts.started_at)) > hours.hour_start
),
raw_events AS (
  SELECT ph.project_id, ph.hour_start, ph.hour_start AS event_at,
         SUM(CASE WHEN unixepoch(attempts.started_at) < ph.hour_start
                   AND unixepoch(COALESCE(attempts.completed_at, attempts.lease_expires_at,
                                         attempts.heartbeat_at, attempts.started_at)) > ph.hour_start
                  THEN 1 ELSE 0 END) AS delta
  FROM project_hours AS ph
  LEFT JOIN work_attempts AS attempts ON attempts.project_id = ph.project_id
  GROUP BY ph.project_id, ph.hour_start
  UNION ALL
  SELECT ph.project_id, ph.hour_start, unixepoch(attempts.started_at), 1
  FROM project_hours AS ph
  JOIN work_attempts AS attempts ON attempts.project_id = ph.project_id
  WHERE unixepoch(attempts.started_at) >= ph.hour_start
    AND unixepoch(attempts.started_at) < ph.hour_end
    AND unixepoch(COALESCE(attempts.completed_at, attempts.lease_expires_at,
                          attempts.heartbeat_at, attempts.started_at)) > unixepoch(attempts.started_at)
  UNION ALL
  SELECT ph.project_id, ph.hour_start,
         unixepoch(COALESCE(attempts.completed_at, attempts.lease_expires_at,
                            attempts.heartbeat_at, attempts.started_at)), -1
  FROM project_hours AS ph
  JOIN work_attempts AS attempts ON attempts.project_id = ph.project_id
  WHERE unixepoch(COALESCE(attempts.completed_at, attempts.lease_expires_at,
                          attempts.heartbeat_at, attempts.started_at)) > ph.hour_start
    AND unixepoch(COALESCE(attempts.completed_at, attempts.lease_expires_at,
                          attempts.heartbeat_at, attempts.started_at)) < ph.hour_end
    AND unixepoch(attempts.started_at) < ph.hour_end
),
events AS (
  SELECT project_id, hour_start, event_at, SUM(delta) AS delta
  FROM raw_events
  GROUP BY project_id, hour_start, event_at
),
points AS (
  SELECT project_id, hour_start, event_at,
         SUM(delta) OVER (
           PARTITION BY project_id, hour_start
           ORDER BY event_at
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
         ) AS concurrency
  FROM events
)
SELECT project_id, datetime(hour_start, 'unixepoch') AS hour_start,
       MAX(concurrency) AS max_concurrency
FROM points
GROUP BY project_id, hour_start
ORDER BY hour_start, project_id;
```

Correlate a pinned result with the mechanism signature before attributing it:

```sql
SELECT project_id,
       strftime('%Y-%m-%dT%H:00:00Z', decision_at) AS hour,
       COUNT(*) AS sampled_refusals
FROM scheduler_decisions
WHERE unixepoch(decision_at) >= unixepoch('now', '-24 hours')
  AND result = 'skipped'
  AND wait_reason = 'provider_rate_window_backpressure'
GROUP BY project_id, hour
ORDER BY hour, project_id;
```

### Per-issue dispatch counts

```sql
SELECT project_id,
       COALESCE(NULLIF(TRIM(identifier), ''), NULLIF(TRIM(issue_id), ''),
                NULLIF(TRIM(issue_url), '')) AS issue,
       COUNT(*) AS dispatches,
       MIN(started_at) AS first_dispatch,
       MAX(started_at) AS latest_dispatch
FROM work_attempts
WHERE unixepoch(started_at) >= unixepoch('now', '-24 hours')
GROUP BY project_id, issue
HAVING issue IS NOT NULL
ORDER BY dispatches DESC, latest_dispatch DESC;
```

### Lane and project distribution

```sql
SELECT project_id,
       COALESCE(NULLIF(TRIM(lane), ''), '(unrecorded)') AS lane,
       COUNT(*) AS dispatches,
       ROUND(SUM(MAX(
         0,
         unixepoch(MIN(COALESCE(completed_at, lease_expires_at, heartbeat_at,
                                started_at), datetime('now')))
         - unixepoch(MAX(started_at, datetime('now', '-24 hours')))
       )) / 3600.0, 2) AS agent_hours
FROM work_attempts
WHERE unixepoch(started_at) < unixepoch('now')
  AND unixepoch(COALESCE(completed_at, lease_expires_at, heartbeat_at, started_at))
      > unixepoch('now', '-24 hours')
GROUP BY project_id, lane
ORDER BY agent_hours DESC, dispatches DESC, project_id, lane;
```

### Blocked-cause breakdown by recovery reason

`workflow_phase_events.reason` is the durable recovery reason on the recorded
entry into Blocked; the alias below matches the operator-facing field name.

```sql
SELECT project_id,
       COALESCE(NULLIF(TRIM(reason), ''), '(unrecorded)') AS recovery_reason,
       COUNT(*) AS blocked_entries,
       MIN(started_at) AS first_seen,
       MAX(started_at) AS latest_seen
FROM workflow_phase_events
WHERE phase_type = 'lane'
  AND lower(phase_name) = 'blocked'
  AND status = 'entered'
  AND unixepoch(started_at) >= unixepoch('now', '-24 hours')
GROUP BY project_id, recovery_reason
ORDER BY blocked_entries DESC, project_id, recovery_reason;
```
