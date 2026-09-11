# Merge Train

[Back to README](../README.md#documentation)

`Merging` is intentionally serialized. Keep this in every production workflow:

```yaml
agent:
  max_concurrent_agents_by_state:
    Merging: 1
```

Do not cap `Todo`, `In Progress`, or `Rework` unless you have a specific
operational reason. Those states should share the global agent pool so workers
stay busy while merge candidates wait for CI or a clean base branch.

When GitHub reports a required native merge queue and the configured gate
permits delegation, Detent delegates every eligible green candidate to the repository's native merge queue instead of rebasing each PR
through the serialized worker. GitHub owns merge-group validation and batching;
Detent keeps the issues in `Merging`, observes their queue entries, and
reconciles them to `Done` after GitHub reports the PR merged. Without a native
queue, a BEHIND PR whose existing head is mergeable and already has green
required checks is submitted to the exact-head merge API without rewriting
the checked head. If GitHub explicitly rejects an out-of-date base, Detent
refreshes that head and waits for its required CI. Other refusals, including
failed checks, conflicts, permissions, changed heads, and native queue
requirements, do not authorize this refresh fallback. If a refreshed head
is missing required contexts, Detent routes the issue to `Rework` instead of
retrying an incomplete merge worker.

A selected merge worker reserves its repository across CI waits for at most
one hour from initial selection. Other same-repository retries cannot refresh
or merge ahead of it during that reservation. Waiting releases the worker and
issue claim, so implementation work and other repositories remain dispatchable
under the configured capacity limits. Dependency priority and queue age select
the next candidate after a reservation is released.

The reservation deadline survives Detent-created head refreshes and restart;
it is stored with the successful waiting work attempt. Failed checks, conflicts,
withdrawal, approval or CI-trigger revocation, native queue handoff, and external
head changes release it. An external base advance keeps the deadline and
requires a fresh correctness check; strict protection may require another
refresh. Missing or degraded tracker data cannot extend the deadline. Expiry
releases the repository even if CI stalls, while the existing CI timeout handles
the waiting issue. Recovery restores only an unexpired reservation whose latest
terminal attempt and current PR still match. A released candidate cannot renew
its own reservation before another candidate takes over. Recorded completion
releases the reservation even after the tracker omits the completed issue from
queue fetches. Diagnostics record
reservation release reasons, head/base identities, and validation invalidation.

Inside the serialized `Merging` lane, avoid duplicating the full local release
gate when it does not buy new signal. If the PR already passed the pre-review
gate, the branch rebases cleanly onto current `origin/main`, and no source files
change during rebase, the merge agent should run a focused rebase/smoke gate
locally and rely on required current-head CI for full enforcement. If the merge
agent edits code, resolves conflicts, detects stale or unknown validation state,
or cannot prove the final rebase was source-clean, it must run the full
configured gate again.

CI waiting should poll current-head REST check runs with backoff, not loop on
GraphQL-heavy PR status commands. Required checks must run on the PR
head before merge; post-merge integration failures are tracked separately.
Merge handoff telemetry should record the
quiet-window wait, GitHub queue/start wait, local merge-gate duration,
current-head PR CI duration, active slow-check runtimes, and whether post-merge
`main` CI is still running. The quiet window, current-head required CI, and
conflict/full-gate fallback are quality gates; repeated full local validation
after a source-clean rebase, noisy status polling, uncached tool install, and
duplicated non-blocking post-merge work are optimization targets.

The repository CI caches the project-pinned golangci-lint binary and only builds
it with `go install` on cache miss. CI builds golangci-lint `v2.9.0` with the
repository Go toolchain so analyzer behavior and toolchain provenance stay
aligned.

PR-required checks are `Lint`, `Verify (ubuntu-latest)`, `Test Coverage`, and
`Browser Visual`. `Security` also runs on every PR. These jobs run for docs-only
PRs too; Browser Visual uses its existing smoke path for nonvisual changes.
The same set runs for merge groups.

`Portability Verify (macos-latest)`, `Portability Verify (windows-latest)`,
`Windows Core`, `Installer Smoke (ubuntu-latest)`,
`Installer Smoke (windows-latest)`, and `GoReleaser Snapshot` run only on pushes
to `main` and manual dispatch. They do not run for PRs, merge groups, tag pushes, or
the nightly schedule. A push to main runs the full set.

Failed integration jobs on main (including manual runs on main) use the existing
machine intake to open a tracking issue or comment on the open match, with
origin kind `doctor` and a stable fingerprint per job name. The issue links to
the failed job and records the commit. These track CI instance health; logs
must establish a product defect before proposing product changes. Infrastructure
failures remain attributed to the instance. Reporting errors fail the reporting job.

The operator must update GitHub's required-checks list to match the four fast
checks above, removing the six integration checks from branch protection and
rulesets. Preserve any separately required Security policy. Changing this
workflow does not change GitHub protection settings.

When implementation workers fill project or global capacity, a clean PR with
passing current-head checks and a known base can complete through the existing
merge completion handler without starting another worker. Each project dispatch
pass allows at most one such synchronous control operation, bounded by a
30-second completion deadline (or the shorter configured merge-worker limit).
Normal queue age and repository reservations select the candidate; claims and
durable attempt ownership still fence completion. Fresh tracker, head/base,
review, approval, and merge checks remain mandatory. A changed head or base
returns to the existing CI wait, and base refresh or conflict work still needs
normal worker admission. Global pauses, scheduling restrictions, lane limits,
provider holds, host pressure, and recovery brakes still apply.

Scheduler decisions report project saturation as `project_capacity_full` and
shared pool saturation as `global_capacity_full`. A project that already used
its control operation reports `ready_merge_control_limit` for another ready
merge when implementation capacity remains full.


## Native queue migration and limits

The queue path requires a nonempty checked PR head. Enqueue sends GitHub's
`expectedHeadOid`; a concurrent push fails admission. Inspection also compares
the current PR head before adopting an existing entry, including after restart.
The entry cache is scoped to that head, so a head change forces inspection.
GitHub removal history also survives restart: an absent entry removed on the
same head is routed to `Rework` with GitHub's removal reason, rather than
automatically re-enqueued after integration failure or cancellation. A newly
checked head can retry. Missing removal identity also routes to `Rework`; an operator can explicitly re-enqueue in GitHub, which Detent
then observes. This preserves removal decisions without a local-only tombstone.
Admission uses the repository queue's `maximumEntriesToBuild` as its window:
when queue depth reaches that limit, further candidates wait without worker
fallback. This leaves the next open slot available to configured priority and
aging instead of enqueueing the entire backlog ahead of a new urgent fix.
Missing or invalid limit metadata defers new admission until a valid window
is observed. Existing provider entries can still be recovered.

Each issue has a merge-attempt budget of two queue entries. The first removal
routes to `Rework` with GitHub's reason; a second removal without a merge
routes the issue to `Human Review` under `merge_revocation_limit` with both
removal reasons in the issue comment. The programmatic path shares the budget:
two identical merge eligibility revocations park the issue. There is no
separate merge fallback duration budget; a conflict-resolution session is
bounded by `agent.max_session_duration_ms` like any other session.

Base advancement alone does not rewrite queued PRs: GitHub validates its new
integration group. Cancelling a dispatch stops further admissions; it does not
withdraw already admitted work from GitHub.

This repository's CI accepts `merge_group: checks_requested`. Checkout uses the
merge-group commit, all required jobs run, and visual detection defaults to the
full visual gate when there is no PR base ref. PR workflow cancellation is
scoped to that PR; it cannot cancel an integration-group run.

Migration is an operator action, never a side effect of starting Detent:

1. Verify every required workflow runs on the merge-group SHA, including any
   required checks outside `ci.yml`. Keep expected GitHub App identities pinned.
2. Preserve required human reviews and stale-review dismissal on head updates,
   failed-check rejection, and all existing protection requirements. Configure
   GitHub's queue with squash merging and a bounded build concurrency and merge
   group size appropriate to the repository's CI capacity.
3. Configure urgent labels through `agent.dispatch_priority_by_label` and retain
   `merge_fairness_age`. Urgency orders admissions; aged ordinary candidates
   retain precedence. Detent does not jump an urgent entry ahead of an active
   group, because that rebuilds already-running integration checks.
4. Trial with two passing PRs and a failing candidate; verify GitHub reports the
   required trusted checks on each integration SHA and ejects failures. Verify
   restart observes entries without enqueueing them again.
5. Keep implementation states uncapped as described above. Native queue waits
   consume no implementation worker. Retain the serialized reservation path
   for repositories without a native queue.

Detent **does not delegate PR-required or security-audit gates**. Their mutable
approval, audit freshness, and revocation rules are not atomically enforced by
GitHub's eventual queue merge. Enabling a repository queue does not remove this
exclusion. A trusted required merge-group policy service with equivalent
revocation enforcement must be designed before migrating such configurations;
pre-enqueue checks or a configuration assertion are insufficient. This includes
the audited dogfood configuration: the changes above prepare supported native
queue use, but do not eliminate that configuration's repeated validation.

A custom aggregate batch is not an equivalent strict-protection fallback.
Passing `base+A+B` does not prove `base+A` passed, and squash merging A changes
the base identity for B. An aggregate PR would need its own reviews and would
change constituent PR semantics.

See [GitHub queue configuration](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
and [GitHub enqueue inputs](https://docs.github.com/en/graphql/reference/pulls).

## Measuring integration retries

Queue diagnostics include entry ID and integration head/base SHAs returned by
GitHub; absent identities remain empty while GitHub prepares an entry.
Correlate recorded work attempts and reservation events with PR head, base, and
CI run identity; count distinct CI runs, not commit rollups or scheduler samples.
Keep source-changing repairs, failed-predecessor rebuilds, and changed integration
inputs separate from repeated validation of identical inputs. A green PR rollup
alone does not establish that its later integration would pass.

The September 8 incident was correlated using the read-only
`detent.before-v0.109.0-20260908T211309Z.db` backup, `work_attempts`,
`scheduler_decisions`, and GitHub Actions runs queried by exact SHA. The sample
window is 18:00–21:13:09 UTC. The unavailable live HTTP endpoint was not needed.

| PR | Recorded work / validation | Interpretation |
| --- | --- | --- |
| #2336 (#2332) | Implementation attempt 4864 ended with green `9491aad6`, base `a862df33`. Merge attempt 4872 refreshed to `28b349d1`, base `1040032d`, and saved a reservation. Attempt 4889 resumed with the same integrated identities and did not push. | One integration refresh, two merge attempts, not two integration reruns. |
| #2336 CI | Original-head run [34266907982](https://github.com/digitaldrywood/detent/actions/runs/34266907982) succeeded; integrated-head run [34270866489](https://github.com/digitaldrywood/detent/actions/runs/34270866489) succeeded. Both have run_attempt 1. | One additional CI cycle after a passing original head; required under strict changed-base validation. No duplicate same-head rerun observed. |
| #2335 (#2333) | No recorded work attempt in this window. Six CI runs belong to `fix/astra-effort-ceiling`: three successful, three cancelled. Each has run_attempt 1. | Work outside recorded attempts must not be counted as Detent redispatches. Cancelled workflow outcomes differ from failed check rollups. |

For #2336, the integrated CI run spans 19:46:00–20:11:17 UTC from creation to
last update (25m17s, not summed job CPU time). Attempt 4872 spans
19:44:59–19:45:55; attempt 4889 spans 20:16:46–20:17:43. Its reservation began
19:44:59 with a 20:44:59 expiry. Eleven current_head_ci_wait decision samples
span 19:46:46–20:13:42. #2333 and #2338 each have eleven reservation samples
behind that same `28b349d1`/`1040032d` owner over that interval. At 19:46:46 the
recorded capacity snapshot has five of ten global slots available and zero of
one Merging slots used. This establishes a repository reservation wait despite
spare worker capacity; sample counts are not dispatch or CI retry counts.

For #2335, successful original `0f894f0c` ran in
[34265532272](https://github.com/digitaldrywood/detent/actions/runs/34265532272).
Integration `c511e44e` ran in
[34268198744](https://github.com/digitaldrywood/detent/actions/runs/34268198744)
(cancelled, with a failed check rollup); fixture repair `b2ea948d` succeeded in
[34268924073](https://github.com/digitaldrywood/detent/actions/runs/34268924073).
Initial `c5cb21d3` and later source correction `c851a471` were also cancelled;
final integration `b91f41b0` succeeded in
[34274232899](https://github.com/digitaldrywood/detent/actions/runs/34274232899).
The apparent fourth successful commit rollup, `61f5eaca`, belongs to another
branch (#2344), not a separate validation of #2335. `c2921a63` has no run.
Source-changing repairs and changed-base integrations are legitimate validation;
the opportunity is avoiding PR-head refresh cycles through provider-owned
integration, not accepting old-base green checks. The available evidence does
not establish any duplicate CI rerun of identical integrated inputs.


## Choosing and enabling a GitHub merge queue

A merge queue validates integration groups against the latest target branch,
reducing repeated PR head refreshes under strict up-to-date protection. The
trade-off is additional merge-group CI and queue latency; required workflows
must support `merge_group: checks_requested`. Repositories without a queue
retain Detent's classic merge path.

Detent reads the default branch's effective rulesets at project load and on
actual workflow reload. Unchanged workflow reconciliation does not read policy.
When PR inspection has no queue entry or GraphQL queue capability, it reads the
PR repository and target branch's effective rules; policies are cached by that
repository/branch pair for five minutes. This also supports delivery repositories
that differ from the issue tracker. A `merge_queue` rule enables native queue
admission. A failed initial discovery retains PR inspection; failed PR-level
discovery defers admission until inspection succeeds. No repository settings
are changed.

In GitHub repository **Settings → Rules → Rulesets**, create or edit an active
branch ruleset targeting the merge branch and enable **Require merge queue**.
Choose squash merging for this project and ensure every required check runs on
the merge-group SHA. See GitHub's [merge queue setup documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue).
The PR-required and security-audit exclusions above still apply.

`detent doctor` recommends considering a queue when strict protection is enabled,
no queue exists, and recorded merge cadence multiplied by median CI duration is
at least 0.25 merges per CI run. It reads the last seven days of lane history,
deduplicates PRs for the repository and target branch, and requires at least
three merges and three measured CI durations. Cadence uses the interval between
the first and last recorded merge. The recommendation reports the sample count,
observed interval, merges/day, median CI minutes, and estimated overlap. CI
durations and target branches are recorded with PR lane transitions; older
history without these fields does not justify a recommendation. Doctor never
mutates branch protection or rulesets.
