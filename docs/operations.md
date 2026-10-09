# Operations report

Read `GET /api/v1/operations` from a runner's local API using the same
authentication and read scope as the state API. The report includes its
generation time and producing instance name.

The JSON structure is shared in `internal/operations`. Historical aggregation
lives in the store; the web layer adds instance identity, current merge-queue
telemetry, explicit human gates, and issue links.

The operator health metric is tokens per merged PR per project per day across
both hosts. Compute it from recorded history, never the dashboard or completion
receipt averages. See [Token spend diagnosis](diagnosis.md#token-spend) for the
authoritative sources, merge definition, and runnable audit queries.

## Stats

The two windows are the trailing 24 hours and seven days, ending at `data_time`.
Historical windows include their start and exclude their end.

- Closes count completed efficiency receipts. Merges use the existing
  cost-per-outcome convention: completed receipts with a positive PR number.
  Per-day values divide those counts by the window duration in days. These
  measurements cover recorded completions, not an independent GitHub audit.
- Cycle time uses the first recorded In Progress entry and first Done entry per
  project and issue, including In Progress entries before the reporting window.
  Repeated transitions do not add cycle samples. Median and p75 use linear
  interpolation. Missing start times are excluded and `cycle_samples` records
  the denominator; no samples produces null percentiles.
- Clean-attempt rate is the fraction of completed receipts with exactly one
  attempt. Tokens per completed issue is the mean total across those receipts.
  Both values are null without completions.
- Blocked nights are UTC calendar dates intersecting the window. Each project
  and issue counts once on each date, even if it enters Blocked repeatedly.
  Dates with no entries are omitted.
- Project dispatch counts use selected scheduler decisions; the five most
  frequent skip reasons are sorted by count, then reason.
- Queue depth sums the reported native queue depth per project, falling back
  to queued Merging work for the serialized path. `merge_group_size` and
  `merge_group_wait_seconds` mirror the repository queue's maximum entries to
  merge and minimum wait when a native queue entry is observed; both are null
  without a queue, and unavailable is not zero.

## Actions and decisions

`since` is an optional RFC3339 timestamp, exclusive, applying only to actions.
It defaults to 24 hours before the current request. Pass the preceding report's
`data_time` to retrieve actions since that render. One reader never consumes
another reader's actions.

Only applied lane-ledger writes stamped with the `operator_routine` origin
appear. The operator routine below produces these writes; the report does not
execute remediations. Each action includes its ledger ID, project, issue, kind,
reason, time, and evidence link.

Decisions include current explicit Workpad human actions and other human gates.
Historical question receipts are excluded from live decisions. Issue and PR
links provide the context needed to act. Decisions are independent of the
action cursor.

## Operator routine

Board remediation runs as one routine on the refresh tick, gated by a
per-project allowlist. It consolidates the former separate loops; there is no
separate tick path for any of them and no external repair script.

```yaml
operator:
  actions: [return_retired_parks, clear_closed_dependencies, restore_stuck_merging]
  merge_wedge_seconds: 7200
```

| Action | Absorbs | Lane reason | Default |
| --- | --- | --- | --- |
| `return_retired_parks` | blocked-cause recovery and recorded-blocker recovery (`tracker.blocked_recovery`) | `blocked_recovery`, `cause_blocked_recovery`, `recorded_blocker_recovery` | on |
| `clear_closed_dependencies` | dependency auto-unblock (`tracker.dependency_auto_unblock`) | `dependency_auto_unblock` | on |
| `restore_stuck_merging` | stale-Merging reconciliation | the existing stale-Merging reasons | on |
| `merge_when_wedged` | manual merge of a gated-green PR when the merge path is wedged | `merge_worker_programmatic_merge` | off |

Each absorbed loop keeps its own configuration and reason vocabulary; the
allowlist only decides whether it runs. Every lane write an action applies is
stamped with origin `operator_routine` and the action kind, which is what the
Actions section reads. An explicit empty `actions` list disables all of them.

`merge_when_wedged` merges only when the issue is in Merging (so its promotion
gate already passed), the current head's CI is green, the PR is open, not a
draft, not dirty, and has no unresolved review threads, the issue has been in
Merging for at least `merge_wedge_seconds`, and neither a native queue entry
nor a merge worker is acting on it. It uses the project's configured
`merge_method` and records the merge under `merge_worker_programmatic_merge`.
Actions never change repository settings and never pause or unpause projects.

`file_deduped_issue` and `apply_doctor_config_fix` from the design are not
available yet; naming them in `actions` is a configuration error.

## Exporting the page

`detent report --html <path>` writes the same report as a self-contained HTML
file for readers away from the runner. It replaces the external
`monitor.py` watcher and `repair.py` repair script, which are retired; actions
now come from the operator routine and are visible in the report itself. A
launchd agent keeps a synced copy current:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.detent-report</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/detent</string>
    <string>report</string>
    <string>--html</string>
    <string>/Users/operator/Dropbox/Detent/Detent Status.html</string>
  </array>
  <key>StartInterval</key><integer>600</integer>
  <key>EnvironmentVariables</key>
  <dict><key>DETENT_API_TOKEN</key><string>replace-with-your-token</string></dict>
</dict>
</plist>
```

On Linux, a systemd user timer calling the same command is equivalent.


## Runner service logs

`detent start` and runner registration install launchd or systemd services with
stdout and stderr appended to `logs/service.out.log` and `logs/service.err.log`
beside the runner's global config. With the default runner config, these are
`~/.config/detent-runner/logs/service.out.log` and
`~/.config/detent-runner/logs/service.err.log`. Installation prints both paths.
Failed service starts are throttled to one restart per minute by the service
manager.

Startup validation is project-specific. A project with unusable tracker
credentials reports its error and repair command through runner problems while
other projects continue serving work. Credential failures use the existing
project retry path, so repairing GitHub CLI authentication allows the project
to start without restarting the runner. If every configured project fails,
the runner reports its problems to the Hub before exiting non-zero.

## Codex disk retention

Detent retains seven days of Codex feedback log rows in the `.detent-worker`
and `.detent-launchd` profiles beneath each configured Codex home. At boot,
after the prior-worker sweep, it deletes older `logs_<N>.sqlite` rows and
vacuums the database. If the database still exceeds 1 GiB, it removes that
log database and its SQLite sidecars; Codex recreates it. Maintenance skips
when any Codex app-server is running or the process list cannot be inspected.
It never stops a process to reclaim disk space. Worker thread state and
thread-history databases are not pruned.

Shared-root rollout deletion is deferred. Worker profiles share the source
Codex home's `sessions` directory, and independent Detent runtime databases
cannot identify another instance's unfinished sessions. The originator field
alone does not establish instance ownership. Detent does not automatically
delete rollouts; the doctor total remains available for operator inspection.

`detent doctor` reports each worker-profile SQLite file size (including
sidecars), the Detent rollout total, and their combined size. It warns above
2 GiB; the doctor check does not delete files. Inspect the sizes before and
after an operator-controlled boot on each host to verify reclaimed bytes.
A host with a running Codex app-server will defer log maintenance.

User-level Codex databases, such as `~/.codex/logs_<N>.sqlite`, are the
operator's responsibility. With Codex closed, its disposable log database and
sidecars can safely be removed and will be recreated. Thread state and history
contain session data: back them up and review what would be lost before manual
pruning. Detent never prunes user-level SQLite databases.

## Profiling

Profiling is opt-in and disabled by default. Add this block to the runner's
`global.yaml` (for an enrolled runner, `~/.config/detent-runner/global.yaml`):

```yaml
profiling:
  listen_addr: "127.0.0.1:6060"
  capture:
    enabled: true
    interval: 15m
    cpu_duration: 30s
    dir: ""
    max_age: 168h
    max_bytes: 1GB
```

The Hub reads the same block from `detent --config /path/to/global.yaml hub
serve ...` (or `CONFIG`/`DETENT_CONFIG`). When no instance config is supplied,
it reads the block from `--hosted-config`, if present. Runners and the Hub
reload profiling settings from their config file without restarting or ending
agent sessions. Invalid reloads retain the previous settings. Listener, capture,
and retention failures are logged through `slog` and do not stop the process.

Leave `listen_addr` empty to disable live endpoints; capture works independently.
The dedicated profiling listener accepts only loopback IP addresses or
`localhost`, which binds directly to `127.0.0.1`. It never shares the application
listener. For a live heap profile:

```sh
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
```

When `dir` is empty, bundles land beside the instance database under
`profiles/orchestrator`, `profiles/runner`, or `profiles/hub`. An explicit directory
can be absolute, home-relative, or relative to the configuration file. Each
interval captures CPU for `cpu_duration`, then writes binary `heap.pprof`,
`allocs.pprof`, `goroutine.pprof`, `mutex.pprof`, and `block.pprof` files alongside
`cpu.pprof` in a UTC timestamped directory. Durations and retention bounds must
be positive; CPU duration must not exceed the interval. `max_bytes` accepts
integer bytes or integer sizes such as `1GB` (decimal) and `1GiB` (binary).

Bundles publish only after all six profiles are written. Canceled or failed
captures remove their incomplete directory. Retention runs at process startup,
when capture starts, and after each capture attempt, removing bundles older than `max_age`, then the
oldest bundles until their combined file size is at most `max_bytes`. Timestamped
`.partial` directories left by a process crash count toward retention; unrelated
directories and symlinks are left alone. A single bundle larger than the size
limit is removed. Use a separate directory per process when overriding `dir`.

Capture enables mutex sampling at one in five events and block sampling at an
average of one sample per millisecond blocked; both reset to zero when capture
is disabled or the process shuts down. Go allows only one CPU profiler at a time,
so a live CPU request overlapping periodic capture can fail; the next capture
interval still runs. Profiles can contain sensitive process data and are written
with owner-only file and bundle directory permissions.

Compare two recorded bundles from the same process using:

```sh
go tool pprof -diff_base /path/to/older/heap.pprof /path/to/newer/heap.pprof
go tool pprof -diff_base /path/to/older/cpu.pprof /path/to/newer/cpu.pprof
```

## Toolchain caches

Workers use the host toolchain caches, including Go's native build and module
caches; no per-project cache choice exists (INV-12). The existing reaper trim
bounds the Go build cache using `global.cache`. `detent doctor` reports the native
paths, current sizes, and last completed reaper trim per project, and warns about
remaining Detent-owned cache roots without modifying them.
