# Refresh dispatch latency evidence (#3674)

The source fixture compares released v0.117.22
(`ff4c950229b679a389f9b04118772c4d84d2af32`) with this patch. It runs the actual
`filterAuthorizedTickIssues`, `operatorReturnRetiredParks` and
`operatorClearClosedDependencies` owners against a migrated SQLite database.
Each observation has 100 excluded active cards (duplicated in fetched and
retained inputs) and 100 genuinely blocked cards sharing an unresolved native
dependency. Each blocked card has a durable Blocked lane entry. Promotion is
not enabled; #3616's ineligible breaker-history exclusion remains effective.

The worker host is Darwin arm64, Apple M4 Max, `GOMAXPROCS=4`, package parallelism
4. These are **first/repeat fixture timings**, not a cold OS filesystem cache
experiment or macbook-air-1 production measurements. Both source versions use
exactly the same workload and owner calls. Each measured operation excludes
migration and fixture construction. Provider methods are local doubles: their
counts describe calls at the connector boundary, **not measured REST/GraphQL
requests or network time**. No higher agent cap is involved.

| Owner | Released first / repeat (ms) | Patched first / repeat (ms) | Released store time first / repeat (ms) | Patched store time first / repeat (ms) |
| --- | ---: | ---: | ---: | ---: |
| Authorization | 15.811 / 19.420 | 5.376 / 4.767 | writes 14.286 / 18.576 | writes 3.936 / 4.024 |
| Retired parks | 24.979 / 23.951 | 23.314 / 23.304 | reads 23.182 / 22.540 | reads 21.510 / 21.934 |
| Closed dependencies | 20.867 / 20.608 | 19.490 / 19.466 | reads 19.209 / 19.022 | reads 17.798 / 17.938 |

Six observations per source on the recovery retry confirm authorization median
15.295 ms released versus 5.940 ms patched (ranges 13.954–19.420 ms and
4.767–6.847 ms). Retired-park medians are 23.953/23.309 ms, and dependency
medians 20.363/19.606 ms. These are descriptive samples, not a production
latency estimate. Timed store writes account for most authorization time;
timed timeline queries account for most recovery time in this local fixture.

| Owner | Timeline reads | Refusal rows | Commits | Identifier calls | Comment calls |
| --- | ---: | ---: | ---: | ---: | ---: |
| Authorization, released | 0 | 100 | 100 | 0 | 0 |
| Authorization, patched | 0 | 100 | 1 | 0 | 0 |
| Retired parks, either | 500 | 0 | 0 | 100 | 0 |
| Closed dependencies, either | 400 | 0 | 0 | 200 | 100 |

Authorization time is dominated by the single-row SQLite evidence writes in
this fixture. The patch removes per-card transaction commits, preserving all
rows, their authorization metadata, aggregate exclusions, and retry release.
`TestAuthorizationBatchRetainsEveryRefusal` checks 300 rejected active cards,
including durable evidence beyond the bounded runtime decision snapshot.
The existing round-trip store fixture also checks all-or-nothing rollback and
ordered IDs for a failing/successful batch. Backends without the optional batch
interface retain single-row evidence writes.

Recovery time in this local fixture is dominated by timeline reads. The patch
does **not** claim those reads or the provider calls disappeared: current
sticky, operator, park, comment and dependency checks remain in their existing
owners. Those two owners' fanout is removed from the unrelated implementation's
pre-dispatch path. Review/visual gates and dispatch's own current dependency hydration still
run before admission. Backend-capacity recovery (which can establish an instance
outage from tracker evidence) and blocker promotion (which can affect dependency
readiness) retain their pre-dispatch positions. Their latency is not measured or
claimed removed here. One rotating dependency check retains first use of the
budget on alternating refreshes; the remaining dependency scan follows dispatch
and excludes the already checked identity. That one priority check can still
wait for provider/store latency. It is not a new timer or recovery loop.

The existing blocked-status owner and missing-retry cleanup run after recovery,
so maintenance sees the same started-work and durable park inputs it saw before
this change. Candidate-specific fresh Blocked status is applied before dispatch.
The existing dispatch owner publishes newly admitted runtime ownership while
the full board snapshot is still awaiting maintenance. REST usage is observed
before dispatch for current capacity authority, then after maintenance; bucket-only observations retain only this refresh's
request summary, and only this refresh's samples are combined and the shared fanout budget is retained.
The configured 15-minute polling interval is unchanged.

## Held operation and explanation deadline

`TestTickDispatchPrecedesBlockedMaintenance` holds an actual dependency-owner
identifier call on a channel with 100 blocked cards and two worker slots (leaving spare capacity). For both alternating
priority states, unrelated approved implementation is running before full
maintenance enters the held call. An unresolved active dependent remains held;
no blocked card is advanced. The public `State` read returns the admitted
worker from the existing runtime publication while that provider call remains
held. The released source fails the same admission-order and runtime-publication
assertions in both priority states. Cancellation releases the held provider
and joins the tick even when a regression assertion fails.

`TestIssueWorkflowTimelineIndexedIdentityUnion/connection_occupied_by_maintenance`
occupies the store's sole database/sql connection, then runs the actual workflow
timeline query. One connection-admission wait consumes the five-second context
and returns `context deadline exceeded` before SQL can execute. This identifies
a reproducible store-contention boundary, not proof of the historical incident's
cause. No network or snapshot-publication wait is involved in that reproduction.
Virtual time avoids waiting five wall-clock seconds or changing production timers.

`TestServiceScopesWorkflowDeadline` gives that same deadline behavior to the
workflow source after a published snapshot has been read. Released source
returns an empty explanation plus `context deadline exceeded` (the web adapter
maps that error to `runtime_unavailable`). Patched source returns schema 3 at
the existing five-second bound, retains the current snapshot/lane and marks
workflow and configured unattempted sources unavailable with existing
`read_failed` evidence. It does not call already-expired sources or claim absent
runtime wiring. Independent source deadlines degrade only their own section;
request cancellation still propagates. Existing web/CLI explanation cases verify
schema-3 transport and degraded DTO behavior. A source that ignores its context
can still exceed the bound; this patch neither increases the timeout nor claims
to interrupt non-cooperative sources.

## Reproduction and validation

Run from this branch with worker-owned scratch output:

```sh
GOMAXPROCS=4 go test -p 4 ./internal/orchestrator -run '^$' \
  -bench '^BenchmarkRefreshMaintenance$' -benchtime=1x -count=6
GOMAXPROCS=4 go test -p 4 ./internal/orchestrator -run \
  'TestTick|TestAuthorization|TestDispatchCandidateStatusBeforeMaintenance|Test.*WorkAttempt|Test.*Scheduler|Test.*Capacity|Test.*Dependency|Test.*Recovery|Test.*RecordedBlocker|TestBlocked.*|Test.*Sticky|Test.*NativePark|Test.*OperatorStop|TestMergingPromotionUsesFetchedAuditIdentity'
GOMAXPROCS=4 go test -p 4 ./internal/store ./internal/explain
GOMAXPROCS=4 go test -p 4 ./internal/web ./internal/cli -run \
  'TestIssueExplanation|TestIssueCommand|TestIssue.*Explain'
true
```

For the released comparison, archive the named release into `$TMPDIR` (or the
provided `$TMP`/`$TEMP`), copy the benchmark plus its single-row `maintenanceStore`
wrapper from `refresh_latency_test.go`, and omit its batch method and regression
tests/imports. Run the same benchmark command. This uses released production
code; no shared worktree/ref or live instance is changed.

Store/explain packages, targeted orchestrator regressions and web/CLI explanation
cases pass. The prior attempt's orchestrator package diagnostics found one independent failure:
`TestRunPausesBackendAfterQuotaErrorWithoutBreakerStrike` expects resetAt + 5s
(~44m away) but gets the fallback five-minute retry. The identical failure was
reproduced on the release and filed separately as #3680. No full-suite,
coverage, race or blocking CI gate was run; the configured gate is `true`.

## Remaining production evidence

The issue's two historical refreshes establish slow steps, not individual
query/network/lock causes. A read-only probe from this worker to
`http://127.0.0.1:4000/` exited 7 immediately with connection refused. No live
process was started, signaled, restarted or replaced, and no live database was
opened or mutated. Therefore production cold/warm comparison, GitHub network
attribution and historical snapshot-publication correlation remain **unverified**.

After Detent integrates the PR and the release owner deploys it, that owner
should compare first/repeat refreshes at the same Parable board workload and
unchanged cap/15-minute interval. Use existing refresh step/REST scope records,
read-only store query timing/connection waits, and schema-3 requests during the
slow operation to separate network latency, store admission/query time and
snapshot age. Confirm unrelated implementation starts before blocked maintenance,
while unresolved dependencies and human/operator/review/visual holds remain.
Deployment/process authorization belongs to the operator; this source worker has
none. Recovery still consumes its recorded reads/provider calls after dispatch;
full refresh and board publication may remain slow. This report claims no
production reduction for that irreducible or unmeasured latency.
