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

## Token spend

The operator health metric is **tokens per merged PR per project per UTC day,
across both hosts** (mac-studio and prometheus in the September 2026 audit).
Use recorded database history, never the dashboard, for this number. Include
spend on unsuccessful and still-open work in the numerator; merged-only spend
answers a different question. Sum host numerators and deduplicate merged
identifiers across hosts before dividing; do not average host ratios. Record
the window, hosts, token source, and merge definition with every result.

### Authority, joins, and time windows

`usage_events` is the authoritative complete source for recorded session token
usage, equivalent to `workflow_phase_events` filtered to
`phase_type = 'agent_session'` in the 2026-09-07..14 audit. Do not add those two
sources together. Complete here means recorded session usage: restart-abandoned
sessions can lack a phase/usage row. `work_attempts.metrics_json` is not an
alternative authority: some rows retain only the last turn. For attempt outcome
dimensions, use `coalesce(phase_event.total_tokens, metrics_json.total_tokens, 0)`
and report the fallback coverage. Preserve validator/routine sessions that have
no attempt when computing all-project spend. Missing metrics mean unknown
spend; the fallback zero is not evidence of free work.

The join keys are:

- `work_attempts.detent_session_id = workflow_phase_events.session_id = codex_sessions.id`.
- `work_attempts.provider_session_id` is the Codex rollout session ID, not the
  Detent session ID. Use it to locate the provider transcript.

Stored timestamps are ISO strings containing `T`. SQLite
`datetime('now', '-24 hours')` emits a space: comparing it directly to these
strings can silently match every timestamp on the cutoff date, including hours
before the cutoff. Bind ISO literals such as `'2026-09-14T00:00:00Z'`, or convert
both sides to epoch values. The copied audit queries below use the explicit
lower bound `'2026-09-07T00:00:00Z'`. Replace every occurrence for another audit;
for a closed window, add the same exclusive ISO upper bound to each underlying
source. Their original window is open-ended, so rerunning later includes newer
rows. Daily spend is assigned by session/attempt start time, not prorated.

### What total tokens means

Codex counts the whole submitted context again on every model call.
`total_tokens` is cumulative processed input plus output, not unique text or
final-answer size. The September 7–14 audit measured roughly 96–98% cached
prefix and 0.3% output. These are observations, not guaranteed ratios. Calls
per session multiplied by context length per call explains most token volume;
a Detent turn may contain many model calls.

Cached reads still meter at one tenth of uncached input for the audited Astra
and Sol rates. Weight tokens by the model's applicable rate before comparing
models; equal token counts do not imply equal spend. Consult the
[Codex pricing page](https://learn.chatgpt.com/docs/pricing#token-rates) for
current rates, billing units, and speed multipliers. With rates per million,
compute `((input_tokens - cached_input_tokens) * input_rate +
cached_input_tokens * cached_rate + output_tokens * output_rate) / 1000000`.
Cached tokens are already included in input; reasoning output is already part
of output. Do not charge either twice or present credits as dollars.

### Rollout layout

On each audited host, transcripts live in
`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`; the worker's isolated
`CODEX_HOME` sessions directory was empty. Inspect the host user's sessions
rather than assuming the worker directory contains the rollouts.

Filter JSONL `session_meta` records to
`payload.originator == "detent-orchestrator"`. Sub-agent threads carry
`parent_thread_id` and duplicate parent history: keep their lineage and exclude
copied history when aggregating calls, text, or cumulative counters. Do not sum
parent and child transcript histories as independent spend.

`event_msg` records with `payload.type == "token_count"` carry
`payload.info.total_token_usage` (cumulative) and `last_token_usage` (last model
call). Do not sum cumulative snapshots; use the final cumulative total or
validated deltas, accounting for inherited history. In `response_item` records,
the first user message after the AGENTS.md item is the Detent prompt, and
`payload.type == "custom_tool_call_output"` denotes tool output. These locations
let an audit separate prompt overhead from tool-output growth.

### Ready-to-paste audit queries

The setup and queries below are copied from the operator's
`~/.detent/token-audit/baseline-2026-09-14/token-audit-appendix.md`, MAC-DB
(Q0a/Q0e, Q2b, Q3b/Q3f, Q4b, Q5a, Q6d). The appendix abbreviates `merged`;
its full definition below comes from the companion `audit_db.py`, with the
window literal substituted. Semicolons are added for interactive SQLite use.
No access to the operator's audit directory is needed to run these copies.

Open a fresh connection on each host and paste the setup once, then any query:

```sh
sqlite3 -readonly "$HOME/.detent/detent.db"
```

```text
.headers on
.mode column
```

Only temporary views are created; the database remains read-only. `att.tokens`
uses phase-event totals with attempt-metrics fallback; `pe.total_tokens` uses
only recorded agent sessions. The merge definition counts distinct identifiers
that reached Done, were delivered, or had a successful merge attempt with the
terminal status message. This is the audit's merged-PR proxy, not an independent
GitHub merge verification; Done can also represent an operator closure.

```sql
create temp view att as
select w.id, w.project_id, w.identifier, w.pr_number, w.lane, w.attempt_number, w.status,
  w.started_at, w.completed_at, substr(w.started_at,1,10) day,
  w.terminal_state, w.error_class, w.error_message, w.status_message, w.wait_reason, w.ci_state,
  w.detent_session_id,
  json_extract(w.worker_metadata_json,'$.run_mode') run_mode,
  json_extract(w.worker_metadata_json,'$.work_product_pushed') pushed,
  json_extract(w.worker_metadata_json,'$.completion_progress.outcome') cp_outcome,
  json_extract(w.worker_metadata_json,'$.completion_progress.reason') cp_reason,
  json_extract(w.worker_metadata_json,'$.completion_progress.progress_kinds') cp_kinds,
  json_extract(w.worker_metadata_json,'$.completion_progress.workspace_diffstat.files_changed') cp_files,
  json_extract(w.worker_metadata_json,'$.completion_progress.tracker_state') cp_tracker,
  json_extract(w.worker_metadata_json,'$.cancellation.reason') cancel_reason,
  json_extract(w.worker_metadata_json,'$.dispatch_loop_start.fingerprint.lane') fp_lane,
  json_extract(w.worker_metadata_json,'$.dispatch_loop_start.fingerprint.pr_head_sha') fp_pr_head,
  json_extract(w.worker_metadata_json,'$.dispatch_loop_start.fingerprint.workspace_head') fp_ws_head,
  coalesce(json_extract(w.worker_metadata_json,'$.dispatch_loop_start.fingerprint.pr_head_sha'),
           json_extract(w.worker_metadata_json,'$.dispatch_loop_start.fingerprint.workspace_head')) dispatch_head,
  json_extract(w.worker_metadata_json,'$.pr_head_sha') end_pr_head,
  json_extract(w.metrics_json,'$.total_tokens') wa_tokens,
  json_extract(w.metrics_json,'$.turns') wa_turns,
  json_extract(w.metrics_json,'$.runtime_seconds') wa_runtime,
  p.id pe_id, p.total_tokens pe_tokens, p.input_tokens, p.output_tokens, p.cached_input_tokens,
  p.reasoning_output_tokens, p.turns pe_turns, p.duration_seconds, p.model_context_window,
  coalesce(p.total_tokens, json_extract(w.metrics_json,'$.total_tokens'), 0) tokens,
  c.agent_role
from work_attempts w
left join workflow_phase_events p on p.session_id = w.detent_session_id and p.phase_type = 'agent_session'
left join codex_sessions c on c.id = w.detent_session_id
where w.started_at >= '2026-09-07T00:00:00Z';

create temp view pe as
select p.*, substr(p.started_at,1,10) day, c.agent_role, c.final_state, c.work_attempt_id
from workflow_phase_events p left join codex_sessions c on c.id = p.session_id
where p.phase_type = 'agent_session' and p.started_at >= '2026-09-07T00:00:00Z';

create temp view merged as
select distinct project_id, identifier from (
  select project_id, identifier from workflow_phase_events where phase_type='lane' and phase_name='Done' and started_at >= '2026-09-07T00:00:00Z' and identifier is not null
  union
  select project_id, identifier from att where terminal_state='delivered'
  union
  select project_id, identifier from att where run_mode='merge' and terminal_state='success' and status_message='worker reached terminal state'
);
```

#### Source reconciliation

Report phase coverage and fallback totals before using attempt dimensions.

```sql
select count(*) attempts, sum(detent_session_id is not null) with_session,
  sum(pe_id is not null) with_phase_event, sum(wa_tokens is not null) with_wa_metrics,
  sum(wa_tokens) sum_wa_metrics_tokens, sum(pe_tokens) sum_pe_tokens, sum(tokens) sum_coalesced_tokens
from att;
```

#### Usage and phase equivalence

The original audit query checks usage rows against their matching phases.
It does not detect phase-only sessions; run the independent check below too.

```sql
select count(*) usage_rows, sum(u.total_tokens) usage_tokens, sum(p.total_tokens) pe_tokens,
  sum(u.total_tokens = p.total_tokens) equal_rows, sum(p.id is null) usage_without_pe
from usage_events u left join workflow_phase_events p on p.session_id = u.session_id and p.phase_type='agent_session'
where u.started_at >= '2026-09-07T00:00:00Z';
```


Check source totals independently and report both unmatched directions and
non-unique session keys before treating the sources as equivalent. Totals alone
can conceal offsetting differences; inspect any nonzero mismatch count.

```sql
with u as (
  select session_id, total_tokens from usage_events
  where started_at >= '2026-09-07T00:00:00Z'
), p as (
  select session_id, total_tokens from workflow_phase_events
  where phase_type='agent_session' and started_at >= '2026-09-07T00:00:00Z'
)
select (select count(*) from u) usage_rows,
       (select count(*) from p) phase_rows,
       (select sum(total_tokens) from u) usage_tokens,
       (select sum(total_tokens) from p) phase_tokens,
       (select count(*) from u where not exists
         (select 1 from p where p.session_id=u.session_id)) usage_without_phase,
       (select count(*) from p where not exists
         (select 1 from u where u.session_id=p.session_id)) phase_without_usage,
       (select count(*) from u join p using (session_id)
         where u.total_tokens is not p.total_tokens) token_mismatches,
       (select count(*) from
         (select session_id from u group by session_id having count(*)>1)) usage_duplicate_keys,
       (select count(*) from
         (select session_id from p group by session_id having count(*)>1)) phase_duplicate_keys;
```

#### Tokens per merged PR per project

This original window-wide query uses all attempt tokens, including fallback,
and separately shows merged-only spend. It excludes sessions without attempts;
use the daily all-session query below for the operator health metric.

```sql
with proj as (
  select project_id, sum(tokens) all_tokens, count(*) all_attempts, sum(pe_id is not null) all_sessions from att group by 1),
mg as (
  select a.project_id, count(distinct a.identifier) merged_prs, sum(a.tokens) merged_tokens, count(*) merged_attempts, sum(a.pe_id is not null) merged_sessions
  from att a join merged m on m.project_id=a.project_id and m.identifier=a.identifier group by 1)
select p.project_id, coalesce(mg.merged_prs,0) merged_prs, p.all_tokens, mg.merged_tokens,
  round(p.all_tokens*1.0/nullif(mg.merged_prs,0)) tokens_per_merged_pr_all_basis,
  round(mg.merged_tokens*1.0/nullif(mg.merged_prs,0)) tokens_per_merged_pr_merged_only,
  p.all_sessions, round(p.all_sessions*1.0/nullif(mg.merged_prs,0),2) sessions_per_merged_pr_all_basis,
  round(mg.merged_sessions*1.0/nullif(mg.merged_prs,0),2) sessions_per_merged_pr_merged_only,
  p.all_attempts
from proj p left join mg on mg.project_id=p.project_id order by p.all_tokens desc;
```

#### Repeat attempts per identifier and head SHA

The dispatch head prefers the PR head and falls back to the workspace head.
Unknown heads are excluded. The copied query groups by a ten-character head
prefix for display; inspect full hashes if prefixes collide. Its sequence is
diagnostic text, not a guaranteed chronological ordering.

```sql
with ranked as (
  select id, project_id, identifier, dispatch_head, lane, terminal_state, tokens, started_at,
    row_number() over (partition by identifier, dispatch_head order by started_at) rn
  from att where dispatch_head is not null)
select project_id, identifier, substr(dispatch_head,1,10) head, count(*) attempts, sum(case when rn>1 then tokens else 0 end) repeat_tokens, sum(tokens) all_tokens,
  group_concat(lane || '/' || coalesce(terminal_state,'active'), ' > ') sequence
from ranked group by 1,2,3 having count(*) > 1 order by repeat_tokens desc;
```

#### Tokens by terminal state and error class

Uses `att.tokens`, including the metrics fallback.

```sql
select terminal_state, error_class, count(*) n, sum(tokens) tokens, round(100.0*sum(tokens)/(select sum(tokens) from att),1) pct from att group by 1,2 order by tokens desc;
```

#### Cache ratio per project

Uses recorded phase events only; the denominator is input tokens.

```sql
select project_id, count(*) sessions, sum(input_tokens) input, sum(cached_input_tokens) cached, round(1.0*sum(cached_input_tokens)/nullif(sum(input_tokens),0),3) cache_ratio,
  sum(output_tokens) output, round(1.0*sum(output_tokens)/nullif(sum(total_tokens),0),4) output_share,
  sum(reasoning_output_tokens) reasoning, round(1.0*sum(reasoning_output_tokens)/nullif(sum(total_tokens),0),4) reasoning_share,
  sum(cached_input_tokens is null) cached_null
from pe group by 1 order by input desc;
```

#### Sessions above 30 million tokens

Uses recorded phase events; missing-phase abandoned sessions are not included.

```sql
select p.project_id, p.agent_role, p.session_id, p.identifier, p.total_tokens, p.turns, p.duration_seconds, p.model_context_window, a.lane, a.terminal_state, a.error_class
from pe p left join att a on a.detent_session_id = p.session_id where p.total_tokens > 30000000 order by p.total_tokens desc;
```

#### Service-restart abandonment clusters

Uses `att.tokens` and groups completion timestamps by minute. `tokens_lost`
is the audit alias for spend on abandoned attempts, not proof that all work
was discarded. Correlate clusters with service logs; infrastructure failures
belong to the instance, never the issue.

```sql
select substr(completed_at,1,16) restart_minute, count(*) attempts_abandoned, sum(tokens) tokens_lost, group_concat(distinct project_id) projects
from att where terminal_state='abandoned' and error_class='service_restart' group by 1 order by 1;
```

#### Daily health metric and host combination

This daily extension of Q1b/Q2b includes all recorded sessions plus attempt
metrics only where a phase event is missing. It counts merge evidence by its
completion/Done day, independently of when the attempt started. Run after the
setup above. Change the start literal here along with the setup. A null ratio
means no merges that day; keep the spend visible.

```sql
create temp view merged_days as
select project_id, identifier, substr(min(merged_at),1,10) day
from (
  select project_id, identifier, started_at merged_at
  from workflow_phase_events
  where phase_type='lane' and phase_name='Done'
    and started_at >= '2026-09-07T00:00:00Z'
  union all
  select project_id, identifier, completed_at
  from work_attempts
  where completed_at >= '2026-09-07T00:00:00Z'
    and (terminal_state='delivered' or
      (json_extract(worker_metadata_json,'$.run_mode')='merge'
       and terminal_state='success'
       and status_message='worker reached terminal state'))
)
where identifier is not null
group by project_id, identifier;

create temp view spend_days as
select project_id, day, sum(recorded_tokens) recorded_tokens,
       sum(fallback_tokens) fallback_tokens
from (
  select project_id, day, total_tokens recorded_tokens, 0 fallback_tokens
  from pe
  union all
  select project_id, day, 0, tokens from att where pe_id is null
)
group by project_id, day;

with days as (
  select project_id, day from spend_days
  union
  select project_id, day from merged_days
), merges as (
  select project_id, day, count(*) merged_prs
  from merged_days group by project_id, day
)
select d.project_id, d.day,
       coalesce(s.recorded_tokens,0) recorded_tokens,
       coalesce(s.fallback_tokens,0) fallback_tokens,
       coalesce(m.merged_prs,0) merged_prs,
       (coalesce(s.recorded_tokens,0) + coalesce(s.fallback_tokens,0)) * 1.0
         / nullif(m.merged_prs,0) tokens_per_merged_pr
from days d
left join spend_days s using (project_id, day)
left join merges m using (project_id, day)
order by d.project_id, d.day;
```

Run against each host's local database, not an aggregate copy that repeats the
same sessions. Export `spend_days` and `merged_days` from the same connection:

```sql
select * from spend_days order by project_id, day;
select * from merged_days order by project_id, identifier;
```

For the both-host report, sum `recorded_tokens` and `fallback_tokens` by
`(project_id, day)` across the host exports. Union the merge exports, group by
`(project_id, identifier)`, and take the earliest day so a merge seen by both
hosts counts once. Count those identifiers by project/day and divide summed
spend by that count. Retain the two token columns so every result states the
source and fallback contribution. Session IDs are host-local; do not join
numeric IDs between hosts. If replicated history is used, deduplicate it by
its originating host/session before summing. Compare like windows and flag
partial days rather than treating today's unfinished work as a regression.

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
