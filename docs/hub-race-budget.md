# Hub race test budget

Run `make test-race` for the complete repository race gate, or
`make test-race-hub` for the complete Hub package. Hub runs separately in three partitions with
two parallel test slots, an uncached race detector run, and a 15-minute
package timeout per partition. Other packages retain Go's default package
concurrency and 10-minute timeout. No test name, assertion, fixture
workload, or individual lifecycle deadline is changed.

## Partitions

The Hub package outgrew one 15-minute race run: merge-group runs of about
380 top-level tests exhausted the package timeout through cumulative
runtime, not a deadlock. Raising the ceiling would hide a stuck test for
longer, so the package is split by top-level test name instead and each
partition keeps the 15-minute ceiling.

`HUB_RACE_PARTITION` in the Makefile holds `^Test[A-GI-O]` and
`HUB_RACE_PARTITION_B` holds `^Test[HW]`. `make test-race-hub-a` passes the
first to `go test -run`, `make test-race-hub-b` passes the second to
`go test -run`, and `make test-race-hub-c` passes both, joined with `|`, to
`go test -skip`. A pattern without `/` matches only top-level names, and the
two `-run` patterns never match the same name, so every top-level test,
example, and fuzz seed runs in exactly one partition by construction,
including tests added later. Subtests always run with their parent.
`make test-race-hub` runs all three partitions in sequence.
`TestHubRacePartitionsCoverEveryTestOnce` pins both patterns, the CI shard
mapping, that no test matches both `-run` patterns, and that no partition is
empty.

CI runs partition A as `Verify race (0)`, partition B as `Verify race (4)`
and partition C as `Verify race (5)`; the other race shards are unchanged.
The letter set was first chosen by running the two original partitions side
by side under the same load with `GOMAXPROCS=2 -parallel 2` (Go 1.26.6,
darwin/arm64) and comparing wall and CPU time; neither summed test elapsed
time nor fixture-open counts predicted wall time well. `^Test[A-GI-O]`
stayed near 545 s on CI through September 2026, while the complement
(`H` and `P` onward) grew past the 900 s ceiling on `main` push runs once
the Change Request landing, review and policy tests landed (`W` about 360 s,
`P` about 250 s and `H` about 225 s of summed elapsed time). Splitting `H`
into its own partition kept every partition under the ceiling; the first
three-way run still put partition C at about 14 minutes of job time and B at
about 5, so the `W` (workspace) tests moved to B. Rebalance by
changing the letter sets when one partition approaches its ceiling; the
complement guarantee does not depend on which letters are chosen.

`make test-race-cover` (used by `make check` and the hourly build) runs the
same three Hub race partitions, then collects one ordinary Hub coverage profile.
The race runs have a 15-minute package timeout each. The one coverage profile
keeps package inputs disjoint for `tools/covermerge`, which rejects overlapping
profiles from multiple runs of the same package.

The `Verify race` jobs keep their 60-minute workflow timeout; the Hub
partitions use at most 15 minutes of it plus setup and evidence upload.
The `Verify (ubuntu-latest)` aggregate requires every matrix shard, so
required check names and branch-protection selection remain the same.

The Hub ceiling is a package budget, not an individual operation deadline.
The recorded Linux/amd64 PR #2292 head passed in 474.738 seconds, then
exhausted 600 seconds on its unchanged retry. Fifteen minutes provides 50%
headroom beyond that exhausted budget and 90% beyond the passing sample.
Separating Hub from other package processes prevents their concurrent race
work from consuming its package budget; two test slots bound fixture
contention on constrained runners. `HUB_RACE_TIMEOUT` and
`HUB_RACE_PARALLEL` are explicit Make overrides for diagnostic experiments.

## Evidence and interpretation

CI uploads each partition's evidence in the `race-evidence-0`,
`race-evidence-4`, and `race-evidence-5` artifacts for 14 days even when
the race step fails.
Locally the same evidence lives in `tmp/hub-race-evidence-a`,
`tmp/hub-race-evidence-b`, and `tmp/hub-race-evidence-c`:

- `combined.jsonl` and `internal__hubserver.jsonl` preserve Go's timestamped
  run, pause, cont, pass, fail, skip, output, and package events, including
  the goroutine dump emitted by Go's package timeout.
- `summary.json` records the package result and budget, race mode, the
  partition's `run` or `skip` pattern, and each
  test's outcome, start/last lifecycle progress, Go-reported elapsed time,
  wall time, and pause-to-cont queue time. An unfinished test remains in
  its last lifecycle state; its observed wall/queue time ends at package exit.
- `hub_fixture_open_seconds` logs measure the complete `openTestService`
  and `openHostedStorage` helper calls, including migration and service
  setup. The summary aggregates their count and elapsed time by test.
  Direct database/service opens outside those helpers are not included.

Queue time includes waiting for a parent to release its parallel children.
Go-reported elapsed time excludes a test's own pause, but parent elapsed
can include waiting for children. Parent and child times overlap, as do
concurrent fixture opens: do not sum these as package wall time. Go JSON
timestamps are observations at the output collector, not a scheduler trace.
Progress output and a timeout classification alone do not establish a deadlock.

For a package timeout, inspect recent completed tests and resume events,
then the active goroutine stacks. Recent fixture starts, changing completed
tests, runnable migration/SQLite stacks, and long parallel queues support
cumulative exhaustion. A test remaining active without completions needs
its own stack and lifecycle investigation; increasing this ceiling does not
resolve a stuck test. Preserve the raw evidence rather than automatically
classifying a package timeout as a deadlock.

## Issue #2307 measurements

The archived failure at PR head `3ac09f851bfc1c491be4c94e7ab32e7c07275f2b`
had four active subtests aged 0–5 seconds when the package reached 600 seconds.
Their stacks included Goose SQL parsing and SQLite schema creation; other
subtests waited in `testing.(*testState).waitParallel`. The same head's
earlier ordinary package tests took 33.271/41.658 seconds.

The local baseline uses Go 1.26.6, Linux/arm64, a Docker CPU quota of two,
`GOMAXPROCS=2`, 6 GiB memory, and a container-local executable tmpfs for
synthetic test fixtures. It is constrained Linux evidence, not an amd64
hosted-runner speed equivalence. The baseline at `7c5396d9` passed all
1,177 selected tests/subtests (three existing opt-in preview skips) in
204.658 seconds. A CPU profile attributed 159.83 of 329.09 sampled CPU
seconds (48.57%) to stacks through `hubserver.runMigrations`.

The first instrumented repeat passed in 209.454 seconds with 467 measured
fixture opens totaling 308.589 overlapping seconds, a maximum queue of
118.770 seconds, and a longest Go-reported test elapsed time of 5.68 seconds.
Repeated validation and the exact recorded-head pilot measurements are
recorded in `.detent/validation/2307/README.md`.

To reproduce independent samples without Go's test cache:

```sh
GOTOOLCHAIN=go1.26.6 GOMAXPROCS=2 go run ./tools/testgate \
  -race -parallel 2 -timeout 15m -run '^Test[A-GI-O]' -output tmp/hub-race-sample-a ./internal/hubserver
GOTOOLCHAIN=go1.26.6 GOMAXPROCS=2 go run ./tools/testgate \
  -race -parallel 2 -timeout 15m -skip '^Test[A-GI-O]' -output tmp/hub-race-sample-b ./internal/hubserver
make check
```

Choose distinct output directories to retain previous samples. Measure
both partitions with the same pattern so their union stays the full
package. The 100-job pilot resides
on still-open PR #2292, so its recorded head is validated separately in an
isolated checkout; this change neither imports nor reduces that workload.
