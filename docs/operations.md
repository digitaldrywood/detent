# Operations report

Open **Operations**, immediately below **Reports** in the sidebar, or read
`GET /api/v1/operations` using the same authentication and read scope as the
state API. The report includes its generation time and producing instance name.
Refresh updates the content in place and preserves the dashboard shell.

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
`data_time` to retrieve actions since that render. The page's Refresh button
carries this timestamp; one reader never consumes another reader's actions.

Only applied lane-ledger writes stamped with the `operator_routine` origin
appear. The operator routine below produces these writes; the report does not
execute remediations. Each action includes its ledger ID, project, issue, kind,
reason, time, and evidence link.

Decisions include published unanswered durable questions and current explicit
human-action gates. Answered questions and unpublished reservations are
excluded. A question is also excluded when the current PR refusal fingerprint
is nonempty and differs from the stored question fingerprint, matching the
orchestrator’s question-wait rule. This filtering precedes gate deduplication
so a superseded question cannot hide a current human-action gate. Issue links, including durable GitHub question-comment links when
available, provide the context needed to answer. Decisions are independent of
the action cursor.

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
file for readers away from the dashboard. It replaces the external
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


## Codex disk retention

Detent retains seven days of Codex feedback log rows in the `.detent-worker`
and `.detent-launchd` profiles beneath each configured Codex home. At boot,
after the prior-worker sweep, it deletes older `logs_<N>.sqlite` rows and
vacuums the database. If the database still exceeds 1 GiB, it removes that
log database and its SQLite sidecars; Codex recreates it. Maintenance skips
when any Codex app-server is running or the process list cannot be inspected.
It never stops a process to reclaim disk space. Worker thread state and
thread-history databases are not pruned.

The existing workspace reaper removes rollout JSONL files older than thirty
days by modification time only when their first `session_meta` record identifies
`detent-orchestrator`. Active attempt references and unfinished or recent
sessions across all projects protect their provider thread IDs, including
thread references recovered from composite thread/turn session IDs. Recent rollout
files or metadata for the same session also protect that session. Unknown or
malformed ownership records and non-Detent rollouts remain untouched. The
shared `sessions` directory may be a symlink; rollout file symlinks are not
followed. Successful sweeps log `removed_bytes` for measurement.

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
