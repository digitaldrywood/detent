# Normal project refresh hydration (#2776)

`TestProjectRefreshHourlyWorkload` invokes `FetchRefreshIssues`, using five
isolated connectors, three Human Review PR-linked issues per project, and 120
ticks at an injected 30-second cadence. HTTP fixture counters are serialized;
client billable/total/304 accounting must match the server counters. Ten cold
projects are also covered. These are synthetic counts, not live-hour or dispatch
acceptance evidence; #2754 retains final workload validation.

Before the change, the reproduced candidate refresh used 4,320 REST / 600
GraphQL requests without validators, all REST billable. Stable ETags yielded
4,320 REST / 420 billable / 3,900 free 304s. The no-validator arm failed the
2,500 billable-REST bound before implementation.

After sharing admission's page evidence and revision-aware PR hydration:

| Five-project hourly refresh | Lane selection | REST | Billable REST | GraphQL | 304 |
| --- | --- | ---: | ---: | ---: | ---: |
| No validators | candidate or observed | 30 | 30 | 1800 | 0 |
| Stable validators | candidate or observed | 30 | 30 | 1800 | 0 |
| Changing validators (cannot return 304) | candidate or observed | 30 | 30 | 1800 | 0 |
| PR observation unavailable, stable ETags | candidate or observed | 9000 | 3645 | 3000 | 5355 |
| Enhanced board schema unavailable, stable ETags | candidate | 4320 | 420 | 1200 | 3900 |
| Enhanced board schema unavailable, stable ETags | observed | 10800 | 3660 | 1200 | 7140 |

The 30 REST requests are cold check-timing details (two per PR); subsequent
complete observations validate their cached revisions. Ten cold projects use
60 REST and 30 GraphQL requests. Complete-evidence counts are asserted exactly.
Fallback arms verify correct results and free-304 accounting, not the optimized
request bound. An unavailable observation still needs legacy detail reads;
observed lanes retain fresh REST reads rather than substituting cached status.

`TestCandidatePRIndependentRefreshEvidence` checks admission, candidate refresh,
observed refresh, and overlapping lane sets after 120 quiet ticks. It preserves
comment edits, native relation additions/removals/closure/reopening, PR
association changes, PR state/head/labels, and checks changing on the same head.
`TestCandidateBatchedPaginationAndAuthority` additionally runs refresh through
comment/dependency pagination and native dependency authority. Existing large
board scans and out-of-lane diagnostics remain covered.
