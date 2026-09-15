# GitHub refresh workload (#2754)

Fixture validation against main `1f574822` (including #2761, #2762, #2763,
#2776). Live-hour validation remains outstanding. No production code changed.

## Reproduce

```sh
go test ./internal/connector/github -run 'TestCandidateHourlyWorkload|TestProjectRefreshHourlyWorkload|TestCandidatePRIndependentRefreshEvidence|TestClientRESTConditional|TestCandidateColdRequestCounts' -count=1 -v
go test ./internal/orchestrator -run '^TestGitHubFleetRefreshWorkload$' -count=1 -v
go test ./internal/connector/github/... ./internal/orchestrator/...
go vet ./internal/connector/github/... ./internal/orchestrator/...
make check-fast
```

## Connector request comparison

Five isolated connectors, three Human Review issues with PRs per repository,
120 refreshes at an injected 30-second cadence (t=0 through 59:30). Cold startup
uses ten fresh connectors with one refresh each. Each has its own HTTP fixture
and caches. No GitHub requests occur. The fixture omits check timestamps to
exercise initial REST fallback followed by observed-detail reuse.

| Workload | REST | Billable REST | GraphQL | 304 |
| --- | ---: | ---: | ---: | ---: |
| Hour, modeled legacy board detail | 2520 | 2520 | 600 | 0 |
| Hour, ReadCandidates board | 30 | 30 | 1800 | 0 |
| Hour, modeled legacy label detail | 3120 | 3120 | 2400 | 0 |
| Hour, ReadCandidates labels | 630 | 630 | 1800 | 0 |
| Hour, pre-#2776 FetchRefreshIssues, no validators | 4320 | 4320 | 600 | 0 |
| Hour, pre-#2776 FetchRefreshIssues, stable ETags | 4320 | 420 | 600 | 3900 |
| Hour, current FetchRefreshIssues, normal path | 30 | 30 | 1800 | 0 |
| Cold ten, modeled legacy board detail | 150 | 150 | 10 | 0 |
| Cold ten, ReadCandidates board | 60 | 60 | 30 | 0 |
| Cold ten, modeled legacy label detail | 160 | 160 | 40 | 0 |
| Cold ten, ReadCandidates labels | 70 | 70 | 30 | 0 |
| Cold ten, pre-#2776 FetchRefreshIssues | 180 | 180 | 10 | 0 |
| Cold ten, current FetchRefreshIssues | 60 | 60 | 30 | 0 |

The pre-#2776 counts were recorded against `b6ddbaca` by the preserved WIP
`b8dd0ff9` during attempt 5911; they are historical fixture measurements.
“Modeled legacy” uses `legacyCandidatePRRefresh`, not a historical build.
Normal current refresh counts hold for candidate and observed-state entry
points with absent, stable, or changing validators. All return three candidates
with passing PR status and no refresh errors. The upstream
`TestProjectRefreshHourlyWorkload` also checks connector accounting against
HTTP response counts. `TestCandidatePRIndependentRefreshEvidence` checks
independently changing comments, native blockers, associations, and PR state
after 120 quiet refreshes through admission and all refresh entry points.

Forced fallback is a separate limitation, not a passing normal-path result:
PR-reference GraphQL failure uses 9,000 REST / 3,645 billable / 3,000 GraphQL
per hour. Missing blockedBy schema uses 4,320 REST / 420 billable / 1,200
GraphQL for candidates, or 10,800 REST / 3,660 billable / 1,200 GraphQL for
observed states. These existing upstream cases assert preserved free 304s,
not the 2,500 normal-operation target.

## Integrated refresh and dispatch

`TestGitHubFleetRefreshWorkload` connects real GitHub connectors to real
orchestrator ticks, eligibility decisions, and start lane transitions. Workers
are blocking in-process test runners. Each project has two permits; all HTTP
clients draw from one fixture REST budget and a separate GraphQL budget,
each capped at 5,000. The transport returns 429 on exhaustion. Stable ETags
return 304 only for a matching If-None-Match, without billable consumption.

Hourly projects each have one Todo item blocked by an open native dependency.
The fixture removes that relation on the final refresh without changing the
issue body; eligibility must change from zero to one and the worker must start.
Every tick asserts board size, eligible count, running count, transition count,
empty refresh error, and absence of lookup backoff. The mixed fleet preserves
four empty boards, reflecting the operator's correction that idle projects
had no admitted work. The ten-project cold case starts with eligible Todo items
and requires ten successful In Progress writes and ten running workers.

| Integrated fixture | REST | Billable REST | GraphQL | 304 | Starts |
| --- | ---: | ---: | ---: | ---: | ---: |
| Five active, 120 ticks each | 4795 | 35 | 680 | 4760 | 5 |
| One active, four empty, 120 ticks each | 959 | 7 | 616 | 952 | 1 |
| Ten active, one cold tick each | 70 | 40 | 60 | 30 | 10 |

All three cases have zero refresh errors, lookup backoffs, 429s, and failed
start transitions. These integrated workloads differ from the PR-heavy
connector comparison above; their counts must not be compared as before/after.
Stable validators still matter: total REST calls remain substantial.

## Limits and live observation

These are deterministic simulated hours, not live-hour evidence. GraphQL
counts are requests; the fixture assigns one cost point per request, not the
real service's query complexity. The fixtures do not reproduce the entire live
project inventory, concurrent agent API traffic, secondary throttling, or
all dispatch-loop/background work. Ten cold projects share a budget but run
sequential ticks; this establishes cold refresh/start behavior, not a burst
concurrency limit. No live process was restarted, replaced, or modified.

On 2026-09-16, this worker's read-only GET to
`http://127.0.0.1:4000/api/v1/state` failed with curl exit 7 (connection refused).
A reachable updated deployment or operator-provided full-hour evidence is
still needed. Do not close #2754 on fixture results alone.

For the live observation, record the deployed version/SHA, start/end UTC,
active project IDs, admitted board counts and available permits, refresh cadence,
and other credential consumers. After confirming all four dependencies are
deployed and cold startup has settled, observe a full 60-minute interval.
Use read-only instance state/history and service journal budget summaries;
do not poll GitHub. Record per-project candidate/eligible/dispatch counts,
refresh errors and lookup-backoff events throughout the interval. Sum REST,
billable REST, 304, and GraphQL requests for that exact interval rather than
subtracting a shared remaining bucket or mixing partial clock hours. Keep the
raw observations in worker-owned scratch and retain a durable summarized
record in the Workpad/report. Required outcome: billable REST below 2,500,
no refresh errors or lookup backoff, and dispatch where admitted eligible work
and free permits exist. Operator's earlier partial v0.114.15 hour is not this
post-#2776 observation.
