# CLI Reference

[Back to README](../README.md#documentation)

## CLI exit codes

Detent uses stable process exit codes so scripts and agents can branch on the
failure class.

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | General or unexpected error |
| 2 | Auth or GitHub token problem |
| 3 | Input validation error |
| 4 | Not found or config conflict |

## CLI JSON error envelopes

When the resolved output format is JSON, command failures write one
RFC 9457-style problem object to stderr. Human-readable pretty-mode errors are
unchanged.

```json
{
  "type": "https://detent.dev/errors/project_not_found",
  "code": "project_not_found",
  "title": "Project not found",
  "detail": "project \"ap\" not found",
  "exit_code": 4,
  "suggested_fix": "available: api, web, infra\ndid you mean \"api\"? see `detent config path`, then retry",
  "did_you_mean": ["api"],
  "docs_url": "https://detent.dev/docs/cli#project-not-found"
}
```

Envelope fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `type` | Yes | Stable problem type URL, using the code slug. |
| `code` | Yes | Stable machine-readable slug. |
| `title` | Yes | Short human title for the error class. |
| `detail` | Yes | Specific failure detail. |
| `exit_code` | Yes | Process exit code for the failure. |
| `suggested_fix` | No | Actionable next step when Detent has a hint. |
| `did_you_mean` | No | Candidate correction list when Detent has suggestions. |
| `docs_url` | No | Documentation URL for the error class. |

Stable JSON error codes:

| Code | Type URL | Exit code | Source |
| --- | --- | --- | --- |
| <a id="general"></a>`general` | `https://detent.dev/errors/general` | 1 | Unexpected error. |
| <a id="validation"></a>`validation` | `https://detent.dev/errors/validation` | 3 | Input validation, invalid config, or invalid output format. |
| <a id="unknown-command"></a>`unknown_command` | `https://detent.dev/errors/unknown_command` | 3 | Unknown command. |
| <a id="unknown-flag"></a>`unknown_flag` | `https://detent.dev/errors/unknown_flag` | 3 | Unknown flag. |
| <a id="github-auth"></a>`github_auth` | `https://detent.dev/errors/github_auth` | 2 | GitHub token or authentication failure. |
| <a id="config-exists"></a>`config_exists` | `https://detent.dev/errors/config_exists` | 4 | `ErrConfigExists`. |
| <a id="project-exists"></a>`project_exists` | `https://detent.dev/errors/project_exists` | 4 | `ErrProjectExists`. |
| <a id="project-not-found"></a>`project_not_found` | `https://detent.dev/errors/project_not_found` | 4 | `ErrProjectNotFound`. |
| <a id="doctor-failed"></a>`doctor_failed` | `https://detent.dev/errors/doctor_failed` | 1 | `ErrDoctorFailed`. |
| <a id="shutdown-forced"></a>`shutdown_forced` | `https://detent.dev/errors/shutdown_forced` | 1 | `ErrShutdownForced`. |
| <a id="shutdown-timeout"></a>`shutdown_timeout` | `https://detent.dev/errors/shutdown_timeout` | 1 | `ErrShutdownTimeout`. |
| <a id="dashboard-unreachable"></a>`dashboard_unreachable` | `https://detent.dev/errors/dashboard_unreachable` | 1 | The configured Detent service is stopped or unreachable. |
| <a id="dashboard-timeout"></a>`dashboard_timeout` | `https://detent.dev/errors/dashboard_timeout` | 1 | The bounded dashboard API request timed out. |
| <a id="dashboard-unauthorized"></a>`dashboard_unauthorized` | `https://detent.dev/errors/dashboard_unauthorized` | 2 | The dashboard API rejected the supplied credential. |
| <a id="dashboard-forbidden"></a>`dashboard_forbidden` | `https://detent.dev/errors/dashboard_forbidden` | 2 | The credential does not allow the requested read. |
| <a id="ambiguous-reference"></a>`ambiguous_reference` | `https://detent.dev/errors/ambiguous_reference` | 3 | The selected project contains more than one matching identity. |
| <a id="issue-not-found"></a>`issue_not_found` | `https://detent.dev/errors/issue_not_found` | 4 | The selected project has no matching issue. |
| <a id="unsupported-model-version"></a>`unsupported_model_version` | `https://detent.dev/errors/unsupported_model_version` | 1 | The CLI and service do not share an issue explanation schema version. |
| <a id="runtime-unavailable"></a>`runtime_unavailable` | `https://detent.dev/errors/runtime_unavailable` | 1 | The running service cannot currently produce the issue explanation read model. |
| <a id="dashboard-request-failed"></a>`dashboard_request_failed` | `https://detent.dev/errors/dashboard_request_failed` | 1 | The dashboard API returned another unsuccessful response. |


## Logging

Detent logs with `log/slog`.

- `ENV=dev`, `development`, or `local` enables tint text logs.
- `ENV=prod` or any other non-development value keeps JSON logs.
- When no environment is configured, Detent defaults to `prod`.
- `LOG_LEVEL` accepts `debug`, `info`, `warn`, `warning`, and `error`.
- `--env` and `--log-level` override environment variables for one run.
- `DETENT_ENV` and `DETENT_LOG_LEVEL` remain deprecated fallbacks for one release. The unprefixed names win when both are set.
- Text logs are written to stdout; JSON logs are written to stderr.
- The terminal dashboard writes JSON logs to `detent.log` next to the runtime
  database. `log_max_size_bytes` defaults to 52428800 and `log_max_backups`
  defaults to 5.
- `detent logs` reads that resolved dashboard log and its numbered backups.
  Headless runs stream JSON or development text logs to stdout or stderr
  instead of a file, so the command reports the dashboard log as unavailable
  when no file exists. It never switches to an arbitrary path or parses text
  logs.
- Log filters use the canonical fields `project_id`, `issue_id`,
  `issue_identifier`, `work_attempt_id`, `detent_session_id`, and
  `provider_session_id`. All supplied filters combine conjunctively. `--since`
  and `--until` are inclusive RFC3339 boundaries compared in UTC, and `--level`
  means the selected level or higher.
- With no overrides, the command examines at most the most recent 8 MiB, keeps
  at most the latest 1,000 matching records in chronological order, and uses a
  24-hour UTC window ending at invocation time. The JSON summary reports byte
  or record truncation explicitly.
- `--output json` writes `{ "records": [...], "summary": {...} }`.
  `--output jsonl` writes one original JSON log record per line to stdout.
  Malformed and partial records are skipped and reported as bounded JSON
  diagnostics on stderr without echoing their contents. JSONL reports
  truncation with an `output_truncated` diagnostic on stderr; the full counts
  are available in the JSON summary.
- Per-request GitHub REST request/response debug logs are off by default. Set
  `tracker.github_rest_debug_logging: true` only while diagnosing REST traffic.

## CLI Output

Detent command output is selected by `--format pretty|json`. The explicit flag
wins, then `DETENT_FORMAT`, then the stdout terminal check. Interactive
terminals default to `pretty`; pipes, redirects, and agent subprocesses default
to `json`. JSON is written to stdout. Progress and logs that would corrupt a
JSON stdout stream are written to stderr in JSON mode.

This changes piped output for scripts that parsed the old prose output. Use
`--format pretty` for a single command or `DETENT_FORMAT=pretty` for a process
environment that must keep the old text shape.

Structured command objects:

| Command | JSON object |
| --- | --- |
| `detent version` | `{"version":"v0.55.0","commit":"abc1234","build_date":"2026-08-01T00:00:00Z","go_version":"go1.26.4","os":"linux","arch":"amd64"}` |
| `detent update` | The update status object, including `current_version`, `latest_version`, `latest_tag`, `update_available`, `install_source`, `action`, `message`, and `command` when present. |
| `detent init` | `{"status":"ok","path":"/path/global.yaml","rule":"--config"}` |
| `detent add-project` | `{"id":"api","workflow":"/repo/WORKFLOW.md","workdir":"/repo","weight":1,"priority":0,"paused":false,"credential_ref":"github"}` |
| `detent pause api --reason "maintenance"` / `detent unpause api` | `{"status":"ok","project":"api","paused":true,"paused_reason":"maintenance"}` |
| `detent resume api --for 2h` | `{"status":"ok","project":"api","active_hours_override_until":"2026-08-07T21:00:00Z"}` |
| `detent promote api --priority 1` | `{"status":"ok","project":"api","priority":1}` |
| `detent remove-project api` | `{"status":"ok","project":"api","removed":true}` |
| `detent work-item add api --title "..." --body "..."` | `{"id":"wi-...","identifier":"wi-...","url":"/projects/api/kanban"}` |
| `detent config path` | `{"path":"/path/global.yaml","rule":"--config"}` |
| `detent auth token enable` / `detent auth token rotate` | `{"url":"https://detent.example.com/?token=..."}` |
| `detent issue '#1643' --explain --project detent` | The exact versioned issue explanation DTO returned by the running service, with no wrapper. |
| `detent doctor` | `{"checks":[{"name":"Config resolution","status":"OK","detail":"...","hint":"..."}],"summary":{"ok":8,"warn":0,"fail":0},"result":"PASS"}` |

### MCP stdio server

`detent mcp` serves the shared read-only operator catalog to MCP-native clients.
It supports the initialization-based MCP revisions `2024-11-05`, `2025-03-26`,
`2025-06-18`, and `2025-11-25`, negotiating `2025-11-25` when a client requests
another revision. The process reads newline-delimited JSON-RPC messages from
stdin and reserves stdout for protocol frames. Logs and command diagnostics go
to stderr. Successful calls use structured content for the 2025-06-18 and
2025-11-25 revisions and one JSON text content block for the older revisions.

The MCP process is an HTTP client of the already-running Detent daemon. It uses
the same config, host, port, wildcard-to-loopback mapping, API-token precedence,
timeouts, and read-scoped authentication bridge as `detent issue --explain`.
It does not open SQLite, start a daemon, or call the tracker. Daemon transport
failures return tool errors rather than empty results. Snapshot-backed results
include `generated_at` and `freshness`; last-known results also include
`expires_at`.

The exposed tools are `board_state`, `fleet_health`, `telemetry_usage`,
`recent_activity`, and `explain_item`. Names, descriptions, input schemas,
limits, and result shapes come from the shared operator catalog and executor.

### Issue explanation reads

`detent issue <ref> --explain --project <project-id>` reads the versioned issue
explanation model from the running Detent service. `--project` is always
required, including for `#number`, so issue numbers are never treated as
globally unique. Issue IDs, canonical identifiers, and full tracker URLs are
accepted as `<ref>`.

The command resolves the configured host and port, maps wildcard bind hosts to
loopback for dialing, and uses `DETENT_API_TOKEN` before the resolved
`api_token`. If either source supplies a credential, the client sends it; a
rejected credential is reported as `dashboard_unauthorized` and is not retried
without authentication. Requests have a ten-second client timeout and honor
caller cancellation.

JSON mode writes exactly one issue explanation DTO to stdout. Pretty mode is a
human-readable projection of the same DTO. Diagnostics and JSON problem objects
remain on stderr.
