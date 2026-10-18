# Instruction activity audit (#3390)

The existing issue timeline endpoint now returns `activity` alongside phase
`events`. Each activity audit includes the safe profile, a wall-time partition,
explicit unobserved intervals, and a stale-checkpoint tail:

```sh
curl --get http://127.0.0.1:4000/api/v1/workflow/timeline \
  --data-urlencode project_id=detent \
  --data-urlencode 'identifier=digitaldrywood/detent#3390'
```

## Reading the evidence

- Identity comes from the existing phase event (project, issue, PR) and its
  profile (central instance database-path digest, work attempt/generation,
  Detent session, provider identity digests, stage). Validator runs do not have
  implementation work-attempt IDs; their explicit attempt reference is their
  independent session, rather than an invented work attempt.
- Instruction references contain SHA-256 hashes of effective prompts and
  bounded startup snapshots of root WORKFLOW.md, AGENTS.md and CLAUDE.md.
  Configuration source versions and snapshot times are preserved. A literal
  command match includes its source line. Private contents are never persisted.
  Native read metadata additionally identifies bounded nested instruction
  snapshots and later versions inside the workspace. Their path digests and
  observation times distinguish source versions. These are recorder snapshots
  after a read request, not proof of the bytes read by the provider; instructions
  outside these sources remain unknown. See [the native evidence limits](diagnosis.md#authority-by-question).
- `observed_read_request` identifies the file requested by a read operation.
  It does not prove successful reading or that an instruction caused later work.
  `inferred_text_match` is a candidate source matching the command literally;
  it may describe a prohibited or obsolete instruction as well as a requirement.
  Reading a file never attributes all subsequent commands to it.
- `head` is the last workspace snapshot the runner already observed, accompanied
  by its observation time and `last_observed_workspace_snapshot` attribution.
  It is not a fresh Git read for every tool. A stale or absent snapshot does not
  establish the exact execution head. Repeat counts group command/tool digests
  within a profile and that observed head, not across attempts or different heads.
  Native action fingerprints also include the provider cwd so identical relative
  operations in different directories do not share action repeat counts.
- The fixed evidence vocabulary identifies Go test/vet/build, known Make checks,
  edits, Git review/rebase/merge, forge merge calls and explicit waits. Raw shell
  text, tool inputs, output, prompts and credentials are absent. Fingerprints
  permit a locally known command to be compared without exposing it.
- Ordered native actions retain individual types, safe identities, candidates
  and repeats within one timed tool span. The first available action list from
  start or completion is retained; repeated completion metadata counts once.
  Actions have no separate durations or outcomes. Compound commands spanning
  multiple activities and opaque code execution stay unclassified. Internal tools
  hidden by a provider are not invented. The tool/turn digest relationship and observed validation-lock child intervals
  preserve nesting; missing wait-end markers do not manufacture wait durations.
- `elapsed_seconds` on a finished span is its observed start/end interval.
  Pending starts report `pending_seconds` separately. Breakdown only credits
  matched completions. Nested child intervals replace their parent interval;
  simultaneous leaf tools share a single `concurrent` bucket. The by-kind sum,
  including `unobserved`, equals wall elapsed rather than summed command time.
- Coverage is always explicitly `partial`: provider events cannot establish all
  agent activity or instruction causality. Drops, unmatched events, unfinished
  tools, unknown intervals and stale tails remain visible. `dropped_events` counts
  received-event loss, `detail_omitted` counts summarized recorder detail, and
  `projection_omitted` counts detail omitted by the bounded public projection.
  These are separate counts, not interchangeable capture failures. Historical sessions
  without profiles remain ordinary phase events, not fabricated empty audits.

## Persistence and policy

A nonblocking bounded queue receives only existing lifecycle observations and
fixed validation markers. Its consumer drains batches (64-event wake threshold,
256-event queue) and checkpoints one phase-ledger row every five seconds, plus
an initial and final checkpoint. Profiles retain at most 1,024 recent spans.
Closed intervals are folded into a fixed-category timing summary before older
detail is removed, so beginning, middle and ending activity remain in whole-attempt
wall-time totals. The public projection retains recent detail within 128 KiB;
summary-only runtime reads retain those totals without loading tool spans.
Earlier gap locations are not retained after compaction. Unfinished intervals
crossing a compaction boundary remain explicitly partial: later completion does
not retroactively establish their earlier timing. Existing discarded historical
profiles cannot acquire missing evidence. Repeat fingerprints remain bounded;
a zero repeat count means the fingerprint could not be retained. Tool input
classification is bounded to 8 KiB and instruction files to 256 KiB each.
Native evidence retains at most 32 actions and 8 KiB including cwd per event,
64 snapshot requests and 64 instruction source records per profile, and four
inferred candidates per action. Truncation and source coverage are explicit.
Saturation increments drop counts rather than delaying tools. Serialization and
SQLite/SSH persistence run outside the worker callback; checkpoint failures log
instance telemetry and do not fail the attempt. The central store serializes a
safe typed profile once and attaches the instance digest.

A process killed between checkpoints can lose the last five seconds or a final
flush. Prior checkpoints survive reopen; unmatched starts and the growing
`unobserved_tail_seconds` expose the missing coverage. The feature uses existing
phase-ledger storage and retention, not a separate transcript or retention loop.
A restarted attempt retains its earlier session profile alongside its new one.

Profiles are excluded from existing lane/session throughput aggregates. No
configuration, CLI command, dashboard, model call, agent prompt, validation gate,
review requirement, tracker lane writer, or project CI policy changes. A no-op
`true` remains a recorded ordinary command; CI projects keep their own gates.

## Sample operator audit

`TestActivityRecorderAuditsActiveAndInterruptedRuns` replays a private fixture in
each agent stage through the real recorder and checkpoint interface. At second
20, the audit has 8 seconds of matched tools and 12 seconds explicitly unknown:

| Kind | Confirmed wall seconds | Evidence |
| --- | ---: | --- |
| Instruction/context read | 1 | AGENTS.md read request; snapshot digest |
| Implementation | 1 | file-change lifecycle |
| Focused local validation | 2 | same command digest, repeats 1 and 2, same observed head; AGENTS.md line 2 and effective workflow match inferred |
| Review | 1 | Git diff lifecycle |
| Rebase | 1 | Git rebase lifecycle |
| Merge | 1 | forge merge endpoint lifecycle |
| Waiting | 1 | explicit sleep command lifecycle |
| Unobserved | 12 | complement of matched intervals |

Validation is the largest confirmed instruction-candidate cost in this fixture.
A third validation start is pending for three seconds and is not credited as
completed validation. At interruption it remains unobserved; the final profile
reports cancellation and retains all eight completed intervals. This is an
isolated acceptance fixture, not a claim about a live operator's costs.
`TestActivityBreakdownPartitionsObservedWallTime` separately establishes that
validation [2,8] and review [4,10] consume eight seconds of wall time, including
four concurrent seconds, rather than twelve sequential seconds.

The live read-only probe on 2026-09-30 could not connect to 127.0.0.1:4000 from
this worker. No live audit or hotfix runtime reproduction is claimed. The
reported original gap (only lane/session totals) is corroborated by the prior
runner path, which only persisted agent_active and contained no activity profile.

## Controlled overhead evidence

Run from the isolated workspace (scratch and databases use its provided TMPDIR):

```sh
go test -p 4 ./internal/runner -run '^$' \
  -bench '^BenchmarkActivityProfileWorkloads$' -benchtime=1x -count=6
```

Measured on 2026-09-30, Darwin arm64, Apple M4 Max, Go 1.26.6, GOMAXPROCS=4.
Six paired samples alternate baseline/enabled order. Baseline omits only the
recorder; both sides run actual /bin/sh tools and existing runner progress
processing. Short = 120 tools; concurrent = four attempts of 60 tools each;
active audit adds a timeline/aggregate query every 10 ms; long = a six-second
command spanning a periodic checkpoint. SQLite migration/setup is outside the
measurement. CPU includes process and child user/system time through final
background flush. Worker time ends before awaiting the flush. Event latency is
recorder enqueue duration, not provider/network latency. These are controlled
local workloads, not a production throughput or API-load forecast.

Medians, baseline / profiling enabled:

| Workload | Commands/s | Command p95 ms | Enqueue p95 microseconds | Total CPU ms | Worker elapsed change |
| --- | ---: | ---: | ---: | ---: | ---: |
| short | 280.60 / 288.65 | 4.002 / 3.942 | 0.125 / 3.083 | 349.578 / 342.499 | -2.781% |
| long | 0.17 / 0.17 | 6009.652 / 6010.205 | 0.188 / 0.521 | 5.239 / 7.157 | 0.009% |
| concurrent | 1048.50 / 1044.00 | 4.393 / 4.271 | 0.083 / 4.958 | 753.667 / 792.208 | 0.450% |
| active_audit | 1064.00 / 1056.50 | 4.193 / 4.352 | 0.083 / 4.583 | 742.841 / 765.314 | 0.682% |

| Workload | Allocated KiB | Allocation count | Process heap MiB | Checkpoint writes | Final metadata / WAL growth KiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| short | 7298.2 / 8039.3 | 8878 / 12348 | 22.18 / 21.39 | 2 | 68.2 / 116.7 |
| long | 62.3 / 171.8 | 102 / 358 | 19.95 / 19.28 | 3 | 1.3 / 60.4 |
| concurrent | 14601.3 / 16120.7 | 17819 / 25150 | 4.06 / 3.37 | 8 | 137.6 / 358.1 |
| active_audit | 14653.5 / 16827.3 | 19121 / 29937 | 9.85 / 3.59 | 8 | 137.6 / 358.1 |

Aggregate incremental enqueue time was 0.067%, <0.001%, 0.469%, and 0.504%
of baseline worker wall time for short, long, concurrent, and active-audit work.
Dividing this same measured duration by baseline CPU gives conservative hot-path
CPU estimates of 0.082%, 0.018%, 0.142%, and 0.153%. Enqueue elapsed includes
scheduler delay, so this is not a separately sampled CPU profile. It supports
the requested hot-path bound in these workloads, not a bound for every provider.

Median worker elapsed regression is <=0.683% and median throughput loss <=0.705%.
Paired short elapsed changes span -8.039% to +4.447%; concurrent spans -7.675%
to +5.041%; active audit spans -2.130% to +4.241%. Six samples on a shared host
cannot prove statistical equivalence at 1%. The observed medians show no material
throughput regression, while tails and noise remain reported rather than hidden.

Total process CPU has a different result: concurrent increases 5.114%, active
audit 3.025%, and the nearly idle long command 36.610% (about 1.918 ms absolute).
Background hashing, classification, serialization, SQLite writes, and audit
queries cost CPU and allocations. We do not claim <1% total-process CPU overhead.
The long-command increase is approximately 0.032% of its wall duration. Heap
values are process-wide post-run samples affected by GC/order, not retained
profile memory or peak RSS. WAL growth includes SQLite pages and ledger revision
updates; final metadata counts bytes retained, not all checkpoint payloads.

Initial experiments exposed per-event consumer wakeups and duplicate checkpoint
serialization. Batching consumption, compacting queued updates, and serializing
once reduced the final median concurrent throughput regression from the first
experiment's 14.149% to 0.429%. That comparison spans different noisy runs and is
not a statistically established optimization percentage.

## Native evidence follow-up (#3655)

The existing timeline returns the largest observed tool intervals with safe
native operation references and instruction candidates. For example:

```sh
curl --get http://127.0.0.1:4000/api/v1/workflow/timeline \
  --data-urlencode project_id=detent \
  --data-urlencode 'identifier=digitaldrywood/detent#3655' | jq '
  .activity[] | .profile as $p |
  $p.spans | sort_by(-(.elapsed_seconds // 0)) | .[:5][] |
  {session_id: $p.session_id, id, kind, fingerprint, elapsed_seconds,
   repeat, attribution, sources, actions, actions_dropped,
   causal_attribution: (.causal_attribution // "unknown_provider_origin")} '
```

An October 1 read-only probe from this worker returned connection refused at
127.0.0.1:4000. No live verification of this unmerged change is claimed. After
integration, Detent's integration owner and the project's release owner retain
live acceptance: use the served build containing this PR, audit representative
Mac/Prom profiles through the existing timeline, rank observed intervals, compare
action repeats and source versions, and keep missing causal provenance visibly
unknown. Deployment or restarting the live instance requires the operator's
explicit authorization. Older profiles cannot recover discarded native fields.

Focused overhead measurements on October 1, 2026, Darwin arm64, Apple M4 Max,
Go 1.27.1 (host toolchain; project target remains Go 1.26), GOMAXPROCS=4:

```sh
go test -p 4 ./internal/runner -run '^$' \
  -bench '^(BenchmarkActivityNativeObserve|BenchmarkActivityInstructionSnapshot)$' \
  -benchtime=200ms -count=3
go test -p 4 ./internal/runner -run '^$' \
  -bench '^BenchmarkActivityProfileWorkloads/native_(short|audit)$' \
  -benchtime=1x -count=3
```

Producer medians (zero/two/32 native actions) are 40.43/161.9/2087 ns per event,
0/256/4352 allocated bytes, and 0/9/129 allocations. Reads, hashing and storage
remain outside that producer path. A 9.5 KiB instruction snapshot on the recorder
goroutine takes a median 51.005 microseconds and 58,136 allocated bytes.

The native workloads emit two actions per tool (an AGENTS.md read and a
printf) at both lifecycle edges, with 120 short tools or four concurrent attempts
of 60 tools plus timeline queries. Three paired samples alternate profiling order.
Worker elapsed excludes final flush; CPU includes parent/children and flush.

| Workload | Commands/s off / on | Enqueue p95 microseconds off / on | CPU ms off / on | Paired median worker elapsed change |
| --- | ---: | ---: | ---: | ---: |
| native_short | 198.6 / 174.9 | 0.125 / 8.584 | 490.326 / 549.570 | +15.244% |
| native_audit | 751.6 / 744.0 | 0.042 / 10.916 | 1047.030 / 1063.849 | +1.027% |

The respective elapsed change ranges are +0.273% to +48.031% and -3.546% to
+1.196% on this shared host. These few noisy samples do not establish production
throughput equivalence or a total-process CPU bound. Final enabled metadata is
about 211/488 KiB; checkpoint counts remain two per attempt (2/8 total). Background
snapshot, serialization and query work has a cost, while producer work stays
bounded and does not synchronously fetch or write. These controlled fixtures
are overhead evidence, not a live audit or proof of instruction causality.
