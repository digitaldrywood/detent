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
and per-project median, p90, and maximum concurrency. Use the recorded activity reads below
when the dashboard needs corroboration or a different window.

## Authority by question

| Question | Authoritative surface | Common trap |
|---|---|---|
| Did throughput or concurrency change? | `work_attempts` intervals, summarized over the relevant window | One `/api/v1/state` response |
| Is a capacity or rate-window mechanism binding? | Its recorded `scheduler_decisions.wait_reason` signature, correlated with `work_attempts` concurrency | Inferring a cause from a low current count or a null live bucket |
| How often did one issue dispatch? | The issue's `work_attempts` history and per-issue timeline | Board or list endpoints, which collapse repeated attempts |
| Why is an issue in this lane? | `workflow_phase_events`, or `/api/v1/workflow/timeline?project_id=<project>&identifier=<identifier>` | The issue's current lane alone |
| Where did an agent spend time following instructions? | Recorded `agent_activity` profiles, their activity intervals, instruction hashes and attribution labels | Treating a file read or an activity category as proof that instructions caused all later work |
| Which Detent build served the page? | The page's `data-detent-served-version` attribute | `detent version`, which identifies the CLI binary invoked from the shell |
| What is true right now? | `/api/v1/state` | Treating that snapshot as historical evidence |
| What does an omitted setting mean? | The loaded configuration and its code default | Treating absence as an unset runtime value |

Read the served build directly from the live page:

```sh
curl -fsS "http://127.0.0.1:<port>/" | rg -o 'data-detent-served-version="[^"]+"' -m 1
```

Activity classification uses the provider's existing parsed command actions
when available, preserving the original command fingerprint. Each tool span
retains an ordered `actions` list with fixed types/categories, hashed type/name/
path identities, action fingerprints, repeats and instruction candidates. A
mixed command retains one timed span: actions have no independent durations or
outcomes. Sort spans by `elapsed_seconds` in the existing workflow timeline API
to identify the largest observed operations; compare `actions[].fingerprint`
and `repeat` under the same workspace head to identify repeated work. Native
action fingerprints include cwd; raw tool command fingerprints remain unchanged.
The first available native list from start or completion is retained once. Compare
`actions[].sources` using `path_ref`, `sha256`, `observed_at` and `matched_line`
to distinguish source versions and inferred instruction candidates. Do not sum
parallel command durations as sequential elapsed time or assign a compound
command's total to every action.

Native instruction reads use the provider's path and working directory (the
workspace when cwd is absent). Native snapshots cover only WORKFLOW.md,
AGENTS.md and CLAUDE.md regular files inside the workspace, including nested
files. The existing recorder goroutine performs these bounded local reads;
dispatch and worker
commands never wait for them. Snapshots are limited to 64 read requests and 64
source records per profile, 256 KiB per file. The queue retains at most 32 actions
and 8 KiB of native metadata including cwd per event; `actions_dropped` reports
truncation. Each action retains at most four inferred instruction candidates.
`source_coverage` explains missing paths, external files, unavailable snapshots
and exhausted limits. File contents, commands, arguments, output and provider
names/paths are excluded from persisted records.

A read request is evidence that an instruction file was requested, not that it
caused later work or succeeded. `recorder_snapshot_after_native_read` records
the version available when telemetry processed the event, not a proof of the
bytes the provider read; edits before queue consumption may change it. Prior
versions remain in `instructions`, while text matching uses the latest local
snapshot. Exact command matches are `inferred_text_match`. Existing native
events supply no instruction-origin provenance, so `causal_attribution` remains
`unknown_provider_origin`, including observed reads and inferred matches.
`coverage_notes`, dropped/unpaired counts and unobserved intervals explain
partial coverage. Older profiles lack structured actions and cannot be repaired
from discarded evidence; absent causal attribution is unknown.

## Hub API latency

Every authenticated `/api/v2` response carries a `Server-Timing` header with
`db`, `compute`, `encode` and `total` durations in milliseconds. `db` is the
time spent executing PostgreSQL statements and reading their rows on any pool,
`encode` is JSON encoding, and `compute` is the remainder of `total`. The
`db` span leaves out `BEGIN`, `COMMIT` and the wait for a pool connection;
that time falls into `compute`.
Reads made of several sub-reads add one named span per sub-read after
`total`, such as `reports_analytics`, `reports_diagnostics`,
`reports_completion`, `reports_residence`, `reports_prior` and the
`analytics_*` parts of the analytics read; spans of concurrent project reads
add up. The `hub request timing` log line of a slow request carries `db_ms`
and the same spans in `spans_ms`. Neither appears in any UI.
Unauthenticated responses carry no timing. One header is a single sample; it
never establishes a trend.

The tenant request-metrics store records per-route latency history: route
template, status class, request count, p50, p95 and max, in hourly buckets
kept for 90 days. Requests only update an in-memory batch, which is flushed to
the metrics store and never to the tenant writer. Organization administrators
read the history through `route_latency` in the `time_series` MCP tool and the
request diagnostics API ([Hub API](hub-api.md)). Slowdown and regression
claims cite this history.

| Read | p95 budget |
|---|---|
| Board and issue reads | under 200 ms |
| Activity | under 300 ms |
| Reports and Diagnostics | under 500 ms |

The Dasha query audit uses production database statistics to identify slow statements. Use the tenant request-metrics history above to identify the affected route and time window.

## GitHub usage timing and privacy

The existing `github rest scope usage` event now includes a bounded `timings`
aggregate alongside its unchanged REST `request_count`, `reserve_refused`,
`fanout_deferred` and `steps`. Correlate existing project/refresh/scope identity
with `project refresh timing` and sub-step completion events for wall elapsed.
GraphQL HTTP attempts are separate from REST requests and quota queries/points;
request count alone cannot establish latency or a saving.

Each timing key uses the existing fixed stage, step, endpoint family and outcome,
plus `Boundary` and an allowlisted GraphQL `QueryPurpose`. Unknown GraphQL purpose
or operation names become `graphql` in this aggregate without changing quota
accounting. Each completed observed boundary reports `AttemptCount`, `TimedCount`,
`ElapsedSumNS` and `ElapsedMaxNS`; elapsed values are nanoseconds. Coverage is
`TimedCount / AttemptCount` within that observed key, never inferred coverage of
all local work. An unavailable timing observation has an attempt but no timed
count or duration. Current instrumented completed attempts are all timed;
in-flight attempts are published when their boundary finishes. Drain clears
counts and timings together; a completion after a drain belongs to the next
outside-refresh cohort. No per-request intervals or rows are retained.

| Boundary | Observed entry paths | Meaning |
| --- | --- | --- |
| `http_transport` | REST/RESTPage, RESTText/RESTTextWithSize, batch REST probe, GraphQL/GraphQLWithType, installation-token mint | Sum of the actual `Do`, body-read/counting and existing deferred drain/close segments. Includes transport and body/close failures, 304 and canceled attempts reaching `Do`. |
| `token_resolution_inclusive` | Existing `Token` invocation in regular REST, REST text/size, batch REST probe and GraphQL request entries, including auth retries | Inclusive resolution with resolver locks/fallback and any nested installation-token HTTP. A cached token creates no HTTP attempt. |

The HTTP clock excludes response logging, progress callbacks, header accounting,
decoding and recursive auth retries between its measured segments. Existing
resource cleanup and request sequencing stay unchanged.
Bounded text reads time only the consumption/close they actually perform;
RESTTextWithSize includes its full-body counting pass. HTTP outcome is the
existing fixed HTTP status classification (`200`, `304`, `429`, `error`), with
body/close failures recorded as timing `error` without rewriting legacy REST
outcomes. HTTP 200 does not prove GraphQL logical success. Token `200` means a
nonempty token was resolved, not that a later HTTP request succeeded. Token
failures or local budget/backoff refusal create no fabricated HTTP observation.

Direct token retrieval outside these request entries, JWT preparation, JSON
encoding/decoding, actor queues and other local work are unmeasured. There is no
independent connection-pool/actor-queue owner. `Do` can include connection-pool
wait, DNS/TLS and server time, so this elapsed is HTTP/transport time, not pure
network time. Sums are overlapping work: two concurrent requests can sum to more
than wall elapsed, and inclusive token resolution can contain installation HTTP.
Never add token and HTTP as disjoint elapsed, subtract their sums from stage wall,
or infer critical path, busy-wall, savings or percentage attribution.

Timing is automatic in the existing aggregate event and requires no detailed
request logs, extra API call, configuration, persistence table or polling.
Persist/log only existing project/refresh/scope identity, fixed names and numeric
aggregates. Raw URLs, query/variables, bodies, headers, credentials/hashes,
command/environment, prompts and private operator criteria are excluded.
Existing unrelated debug behavior is unchanged; it is not needed for timing.

## Token spend

Token efficiency is measured as tokens per merged PR per project per UTC day,
from recorded database history rather than the dashboard. The numerator includes
spend on unsuccessful and still-open work; merged-only spend answers a different
question. When several runners contribute, sum their numerators and deduplicate
merged identifiers before dividing; averaging per-runner ratios is not the same
metric.

### Authority, joins, and time windows

`usage_events` is the complete source for recorded session token usage. It is
equivalent to `workflow_phase_events` filtered to `phase_type = 'agent_session'`;
the two must not be added together. Sessions abandoned by a restart can lack a
phase or usage row. Attempt-level token metrics live inside the opaque
`work_attempts.metrics_json` payload, which SQL does not read (INV-20); attempt
reads through the API return it whole. Attempts without a matching phase event
therefore have unknown token usage in SQL, never zero.

The join keys are:

- `work_attempts.detent_session_id = workflow_phase_events.session_id = codex_sessions.id`.
- `work_attempts.provider_session_id` is the provider rollout session ID, not the
  Detent session ID. Use it to locate the provider transcript.

Typed attempt columns cover the fields the queries below need:
`metadata_run_mode`, `metadata_pr_number`, `metadata_pr_head_sha`,
`metadata_pr_base_sha` and `metrics_turns`. Older rows are projected in the
background one row per transaction; `projection_ready IS NULL` marks a row not
yet projected, so report that count with every result.

Ordered timestamps use PostgreSQL timestamp types. Bind typed UTC boundaries
and test both ordering and the boundary predicate. Daily spend is assigned by
session start time, not prorated.

### What total tokens means

Codex counts the whole submitted context again on every model call.
`total_tokens` is cumulative processed input plus output, not unique text or
final-answer size; cached prefix typically dominates it. Calls per session
multiplied by context length per call explains most token volume, and a Detent
turn may contain many model calls.

Weight tokens by the model's applicable rate before comparing models; equal
token counts do not imply equal spend. See the
[Codex pricing page](https://learn.chatgpt.com/docs/pricing#token-rates) for
current rates. With rates per million, cost is
`((input_tokens - cached_input_tokens) * input_rate +
cached_input_tokens * cached_rate + output_tokens * output_rate) / 1000000`.
Cached tokens are already included in input and reasoning output is already part
of output.

### Rollout layout

Codex transcripts live in the host user's
`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`; a worker's isolated `CODEX_HOME`
sessions directory may be empty. Filter JSONL `session_meta` records to
`payload.originator == "detent-orchestrator"`. Sub-agent threads carry
`parent_thread_id` and duplicate parent history, so parent and child histories
are not independent spend. `event_msg` records with
`payload.type == "token_count"` carry `payload.info.total_token_usage`
(cumulative) and `last_token_usage` (last model call); cumulative snapshots are
not summed.

### Audit reads

Use the Detent MCP attempt, report and history reads for the authenticated
organization and project. Runners keep no durable application database.
Correlate attempts by their typed identities and recorded timestamps, and state
the time window and coverage of every result. Use PostgreSQL diagnostics only
when the supported application reads cannot answer the question; never infer
runtime history from a disposable workspace.

## Codex startup stalls

A `backend_startup_timeout` identifies a missed handshake deadline, not a
provider outage. The `codex app-server startup failed` log event contains
payload-free `startup` evidence even for admission runs. Issue attempts also
persist it as `backend_startup` in the attempt's worker metadata, which attempt
reads return whole.

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
Retries retain workspace state and the existing instance retry backoff.
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
   response/connection evidence establishes an outage. The existing instance
   breaker limits repeated pre-turn failures.

## Workspace scan deadlines

A `workspace scan command stage=wait` error means the command started but the
scan did not complete before cancellation. A short `start_elapsed` measures
`cmd.Start`, not completion of executable initialization. When cancellation
returns before `cmd.Wait`, `output_bytes=unknown` means the waiter still owns
the output buffer. Older builds reported `output_bytes=0` in that case even
when a child had written output; do not infer silence or a startup stall from
that historical value.

For a recurrence, record the error and command PID, then take a bounded
process sample while the affected command is still running. Keep draining
stdout concurrently, or redirect an isolated probe to a temporary file:
pausing a pipe reader while sampling can itself block `lsof` in
`__write_nocancel`. Such probe-induced backpressure is not evidence of the
original failure. Record completed output size, exit status, elapsed time,
and relevant stack symbols, keeping process paths and raw samples private.

## Recorded activity reads

Use the operator activity report and attempt history reads with an explicit UTC
window and project scope. Compare recorded attempt intervals, dispatch events,
lane transitions and outcomes. Report missing or incomplete intervals alongside
concurrency, throughput and blocked-reason claims. A current state snapshot does
not establish historical delivery or concurrency.

## Security audit waits

`security_audit_runs` stores completed results, not launch records. Its
`completed_at` field is required. Absence of a row cannot establish that an
audit did not start. Correlate the wait timestamp with `codex_sessions` where
`agent_role = 'security_audit'`, including `started_at` and `completed_at`.
Audit scratch paths alone are insufficient: allowance triage also uses the
security-audit scratch root.

The in-memory stage registry prevents duplicate launches while a reviewer is
active. It is initialized empty on restart. Missing exact-base/head results
request an audit on the existing auto-promote pass, including Merging; a
trusted completed result remains usable after restart. Re-running a completed
passing audit is not repeated merely because the process restarted.
