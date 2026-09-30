# SQLite refresh I/O profiling (#3235)

## Method and scope

Profiled September 30, 2026. A read-only connection to prometheus produced an
online SQLite backup in memory; only its serialized backup was transferred to
the worker's provided scratch directory. No live database, service, process, or
port was changed. The source database was 747,065,344 bytes, with 13 projects in
lane observations, 15,566 sessions, 8,251 work attempts, 189,719 scheduler
decisions, and 217,460 workflow events. Scheduler rows occupied 442,716,160
bytes; workflow rows occupied 93,077,504 bytes.

The before binary was built at `c85519e3a0b85e076cc12127f5c94ce31e11c4fe`
with the new benchmark only. Both binaries used Go 1.27.1, linux/arm64 and
`modernc.org/sqlite` v1.51.0 in isolated Alpine containers. Both replayed the
same backup, restoring migration 64 in the disposable comparison fixture for
the before run, then applying migration 65 for the after run. Setup, online
backup, index creation and fixture selection are outside the counters/timing.
Six refresh iterations were measured for each binary.

`BenchmarkSQLiteRefresh` reads 500 recent scheduler decisions for each of ten
projects and lifetime tokens, resume state, attempts and card history for 30
lookup identities per project (300 total). Historical identities are used when
available; missing slots deliberately exercise issues without history. This is
an isolated replay of the store work, not a complete orchestrator, tracker, or
live production run. Without an incident-backup path the benchmark generates
ten projects, 300 lookup identities, 18,000 sessions and 180,000 wide scheduler
decisions. The backup and its private rows are not committed.

## Results

| Per refresh | Before | After | Reduction |
| --- | ---: | ---: | ---: |
| `rchar` | 17,723,565,237 B | 336,590,010 B | 52.7x |
| `wchar` | 14,479,360 B | 2.667 B | sort writes eliminated |
| `write_bytes` | 14,479,360 B | 0 B | sort writes eliminated |
| Time | 7.667 s | 0.426 s | 18.0x |

Raw results: [before](benchmark-before.txt), [after](benchmark-after.txt).
The tiny after `wchar` is benchmark output, not database persistence.
At an **assumed** 30-second refresh interval, the measured counters extrapolate
to 2.127 TB/h versus 40.391 GB/h of logical reads, and 1.738 GB/h versus zero
refresh-sort writes. These are projections, not one-hour observations. Active
worker writes, real transitions, scheduler decision inserts, board JSON snapshots,
other analytics and a complete fleet's concurrent workload are not measured by
this replay. The evidence exceeds the issue's 10x target for the identified
refresh reads and writes; it does not establish the entire live process's
post-deployment write rate or attribute all of the original 60 GB/h incident.

## Hot queries and remedy

Single-query probes use the same backup with Python's SQLite driver for
`EXPLAIN QUERY PLAN` and Linux I/O counters. They supplement the production
driver replay, which is the source of the numbers above. The before and after
recent-scheduler probes differ in scope: the old optional project filter versus
the new fleet query; the production replay uses ten project-scoped queries in
both runs. See [before plans](query-plans-before.json) and
[after plans](query-plans-after.json).

- The old optional-project recent-scheduler query scanned 194 MB and sorted wide
  rows even for a single project. The production-driver replay spilled these
  sorts, writing 14.5 MB per refresh. Separate fleet/project queries use the
  matching time index and apply the limit without sorting payloads.
- Session token, latest-session and resume queries scanned the 13.4 MB session
  table per issue. Their guarded `COALESCE`/`OR` predicates hid identity indexes.
  Indexed identity unions now select row IDs; outer reads use the integer
  primary key. SQLite initially preferred project/time scans even with an ID
  subquery, so the outer table uses `NOT INDEXED`, which preserves integer
  primary-key lookup while excluding those competing secondary indexes.
- Card history and attempt reads similarly re-read project history per issue.
  Direct, project-scoped identity unions preserve alias matching, deduplicate
  rows, and fetch only the matching records before counting or sorting them.
- Issue activity scanned scheduler, workflow, attempt, session and usage tables;
  one probe read 359 MB. Identity unions and identity-first indexes remove these
  scans, including the legacy reader with no project scope. Equivalent
  project-first alias indexes are replaced rather than duplicated.
- Workflow analytics sorted historical payloads although Go already determines
  report and representative ordering. Those SQL sorts are removed. Reads use
  the existing finished-time index to skip ongoing activity payloads. Full cold
  analytics still read their required history; this does not introduce a cache.
- No `VACUUM` or `wal_checkpoint(TRUNCATE)` loop was found. The live process had
  the runtime database open, not the separate GitHub-local tracker database, so
  GitHub-local upserts are not attributed as this incident's measured cause.

## Regression and diagnostics

`TestRefreshHistoryQueriesUseIndexes` captures SQL executed through the real
store methods, verifies indexed identity/primary-key searches, and rejects wide
recent-scheduler sorts. It fails for scans or project-history fetches even when
the returned results remain correct. Existing project-scope fixtures now cover
ID, identifier, URL, and overlapping aliases; the correlation fixture also
asserts report equivalence with reversed historical input and unique row IDs.

Passed: `make generate`; generated-fixture benchmark smoke (one refresh,
57,487,489 logical bytes read and zero measured writes); focused refresh, identity, card history, workflow,
efficiency and scheduler tests; budget/explain package diagnostics; focused
orchestrator/web lifetime, resume, spend, card and workflow consumers. Configured
gate: `true` (no validation/status); required status checks: empty. No full
repository suite, coverage gate, race suite or browser verification was run;
this changes database work without changing the UI.

The store package diagnostic failed only at the pre-existing
`TestCompletionFenceRevocationMigrationAndAccounting`: its migration-54 fixture
seeds through a usage INSERT containing migration-64 `cpu_seconds`. The exact
failure, `table usage_events has no column named cpu_seconds (1)`, reproduces in
the unmodified before binary. Follow-up: digitaldrywood/detent#3533, fingerprint
`store-completion-fence-fixture-missing-compute-usage-columns`.

To run the generated benchmark fixture on a Linux host:

```sh
go test -p 4 ./internal/store -run '^$' -bench '^BenchmarkSQLiteRefresh$' -benchtime=6x
```

For an isolated online backup already under provided worker scratch:

```sh
DETENT_SQLITE_PROFILE_DB="$TMPDIR/isolated-backup.db" \
  go test -p 4 ./internal/store -run '^$' -bench '^BenchmarkSQLiteRefresh$' -benchtime=6x
```

Never set that benchmark-only variable to a running instance's database.
