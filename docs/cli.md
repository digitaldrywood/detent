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
| `detent promote api --priority 1` | `{"status":"ok","project":"api","priority":1}` |
| `detent remove-project api` | `{"status":"ok","project":"api","removed":true}` |
| `detent work-item add api --title "..." --body "..."` | `{"id":"wi-...","identifier":"wi-...","url":"/projects/api/kanban"}` |
| `detent config path` | `{"path":"/path/global.yaml","rule":"--config"}` |
| `detent auth token enable` / `detent auth token rotate` | `{"url":"https://detent.example.com/?token=..."}` |
| `detent doctor` | `{"checks":[{"name":"Config resolution","status":"OK","detail":"...","hint":"..."}],"summary":{"ok":8,"warn":0,"fail":0},"result":"PASS"}` |
