# CLI Reference

[Back to README](../README.md#documentation)

## Commands

Use `detent --help` for the top-level commands and `detent help --format json`
for the full command catalog, including subcommands, flags and examples.

| Area | Commands |
| --- | --- |
| Hub and runner | `hub`, `cloud`, `attach` |
| Artifacts and installation | `artifacts`, `update`, `version`, `skill` |
| Configuration and diagnostics | `config`, `doctor`, `fix`, `logs`, `help`, `completion` |
| Project operations | `pause`, `resume`, `budget`, `capacity`, `exposure` |
| Application reads and tools | `audit`, `report` |
| Isolated browser fixtures | `dev-runtime` |

Enroll a runner with `detent hub runner register --service` to install and start
its background service. On Windows this is a per-user logon Scheduled Task;
see [Windows runners](#windows-runners) for native and WSL2 setup.
Run `detent --config /path/to/global.yaml --headless`
to start an enrolled runner in the foreground. Hub API keys are managed in
Settings > API & MCP.

`config migrate` and `fix workflow-layout` remain available for repository
workflow migrations. `dev-runtime` remains available for `tests/visual` and
Browser Visual CI fixtures.

## Hub operator access

Use the Hub's authenticated HTTP MCP endpoint or scoped APIs for operator work.
The following operator commands are removed; `hub issue import-local` remains for local migration:

| Removed command | Hub replacement |
| --- | --- |
| `issue --explain` | MCP `explain_item` |
| `hub issue` issue and Change operations | MCP `work_item`, `file_issue`, `edit_item`, `move_item`, `add_comment`, `edit_comment`, `set_dependency`, `work_comments`, and `work_history`; native Change and review-policy APIs under `/api/v2/organizations/{organization}/projects/{project}` |
| `state` | MCP `board_state` and `dashboard` |
| `promote` | MCP `organization_project_rank` and `organization_project_rank_update`, or `GET` and `PUT /api/v2/organizations/{organization}/project-rank` |
| `unpause` | MCP `resume_local_project`, with the selected runner and current configuration, policy and runner revision preconditions |
| `mcp` | HTTP MCP at `/api/v2/organizations/{organization}/mcp`; hosted shared entry also serves `/mcp` |

See [API and MCP setup](api-mcp-setup.md) for credentials and client configuration,
and [Hub API](hub-api.md) for native issue, Change and review-policy routes.
Project rank changes require the existing organization administrator authority;
runner routing and resume operations require runner administration.

Commands with capabilities absent from the Hub remain available:

| Retained command | Capability preserved |
| --- | --- |
| `pause` | Local pause reasons and automatic unpause at a timestamp or after a tracker issue closes. The Hub has no equivalent conditional project-dispatch pause. |
| `resume` | Temporary dispatch outside active hours using `--for` or `--until`; this differs from `resume_local_project`, which clears a runner project's pause. |
| `report` | Self-contained HTML operations export. Hub MCP `reports` supplies structured analytics, without this export. |
| `hub issue import-local` | Reads a host-local SQLite copy, maps lanes and imports records with preflight and replay support. The Hub import API accepts converted records; it cannot read the host file. |
| `logs` | Bounded filtering of host log files and rotated backups. Headless runner logs remain on the host's stdout/stderr or service log destination. |

These retained local operations use the existing runtime or configuration.
Use Hub MCP for native work management. `hub issue import-local` is the migration
bridge described in [Local Hub migration](hub-local-migration.md).

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
| `detent pause api --reason "maintenance"` | `{"status":"ok","project":"api","paused":true,"paused_reason":"maintenance"}` |
| `detent resume api --for 2h` | `{"status":"ok","project":"api","active_hours_override_until":"2026-08-07T21:00:00Z"}` |
| `detent config path` | `{"path":"/path/global.yaml","rule":"--config"}` |
| `detent exposure [--project api]` | `{"findings":[{"source_project":"api","source_repository":"example-org/private-api","destination_repository":"example-org/public-project","issue_number":42,"issue_url":"https://github.com/example-org/public-project/issues/42","matched_identifier":"example-org/private-api"}],"warnings":[]}` |
| `detent skill install --target codex --dry-run` | The complete skill install result, including bundle/build stamps, target intent and status, every planned filesystem action, and any rollback actions. |
| `detent doctor` | `{"checks":[{"name":"Config resolution","status":"OK","detail":"...","hint":"..."}],"summary":{"ok":8,"warn":0,"fail":0},"result":"PASS"}` |
| `detent fix worker-processes --dry-run` / `--yes` | The orphaned process groups reported by the live service, including session identity, PID, PGID, RSS, and the reap outcome when applied. |

### Doctor token diagnostics

`detent doctor` includes these read-only project checks:

| Check | Evidence and thresholds |
| --- | --- |
| `workflow_source_drift` | Warns when `workdir` is on a different branch from the GitHub tracker default branch with no `workflow_ref`, or when the pinned WORKFLOW.md differs from the working copy. Reports both byte sizes; missing refs or metadata warn. |
| `instruction_budget` | Estimates the repository AGENTS chain (root to workdir, override-file precedence, **32 KiB aggregate cap**), effective WORKFLOW prompt, and literal file paths (filenames with extensions, `Makefile`, or backtick-quoted paths) on read/follow/consult/load/review directive lines in every loaded instruction file, including transitive references (deduplicated). Lists files and bytes; **warns above 24 KiB, fails above 48 KiB**. Missing evidence warns. |
| `gate_instruction_conflict` | Warns when `gate.run` is required in multiple instruction files or both required and forbidden/conditional. Uses the same instruction inventory and line-based requirement/prohibition heuristics. When configured for `make check-fast`, also audits conflicting full `make check` instructions. |
| `model_policy` | Prints each effective level's model, effort, and configuration sources. Warns when **more than 10%** of project `codex_sessions` started in the last 24 hours have `issue.effort` (including role-specific `issue.<role>.effort` sources) above the current level default, or provenance cannot be compared. |
| `token_accounting` | Compares project `usage_events.total_tokens` with `work_attempts.metrics_json.total_tokens` over the last 24 hours; warns when the absolute difference is **more than 5% of usage_events**. Zero usage with nonzero attempt totals also warns. |
| `ci_trigger_shape` | For GitHub trackers, reads default-branch workflow files in one GraphQL query. Warns when a configured required-check job runs on an explicit `labeled` activity without a workflow/job concurrency group containing `github.event.pull_request.head.sha`. Missing or unreadable required workflows warn. |

Instruction analysis is a static estimate, not a full natural-language or Codex
configuration interpreter. It excludes dynamic template expansions, dynamic file
paths, custom Codex fallback filenames, and instructions below the configured
workdir; isolated workers exclude user-home instructions. Referenced files are
read from the checkout, while the workflow prompt uses the configured source.
Model history is compared against today's effective policy, not historical config.
Accounting uses usage finish times and attempt completion times (start time for
unfinished attempts); window-crossing sessions can produce differences. Use
`usage_events` as the reconciled reporting source. Unavailable database evidence
warns; these diagnostics do not change policy, tracker lanes, or runtime state.

### Operator skill installation

`detent skill install` installs the embedded read-only operator introspection
skill for explicitly selected local agent clients. `--target` is required and
repeatable; supported values are `claude-code`, `codex`, and `antigravity`.

| Target | Discovery scope | Deterministic installed entrypoint |
| --- | --- | --- |
| `claude-code` | Claude Code personal skill | `~/.claude/skills/detent-operator-introspection/SKILL.md` |
| `codex` | Codex user skill | `~/.agents/skills/detent-operator-introspection/SKILL.md` |
| `antigravity` | Antigravity global skill | `~/.gemini/config/skills/detent-operator-introspection/SKILL.md` |

The locations intentionally follow each client's own discovery contract. The
portable bundle body does not imply a shared client home layout, and the
installer never writes repository `.detent/skills` worker metadata.

Use `--dry-run` to validate all selected roots and print every directory,
managed file, backup path, and action without writing. JSON output returns the
same complete plan and result model. The command preflights every selected
target before applying any action, so a conflict or unsafe path prevents all
targets from changing.

An exact reinstall is a no-op. A changed skill, changed install manifest,
upgrade, downgrade, or partial prior install fails without `--force`; the
command does not prompt in JSON mode. `--force` first creates deterministic
content-addressed backups beside changed managed files, then atomically replaces
only `SKILL.md` and `.detent-install.json`. Other files in the skill directory
are preserved. If a later action fails, earlier managed-file and directory
changes are rolled back across all selected targets, and rollback results are
included in output.

Every existing path component from the user home through the destination must
be a real directory. Symlinked roots, directories, managed files, backup files,
and lexical escapes are rejected. New directories use mode `0700`; managed and
backup files use mode `0600`. The install manifest records the embedded bundle
version and SHA-256 plus the running binary's resolved build version, commit,
date, and dirty state. It contains no credentials, Detent config, or runtime
filesystem paths.

### Operations report export

`detent report --html <path>` reads `GET /api/v1/operations` from the running
service and writes it as one self-contained HTML file: inline styles, no
scripts, no external assets, so it reads correctly from a synced folder or an
email attachment away from the tailnet. The page states the producing instance
and its data time and refreshes itself every five minutes when left open in a
browser. Service-relative evidence links are rewritten to the service origin.
The file is written to a temporary sibling and renamed into place, so readers
never see a partial page. Pretty output reports the path, instance, data time,
and size; `--format json` returns the same fields.

## Provider capacity recovery

After restoring subscription usage, request full configured dispatch capacity:

```sh
detent capacity clear --scope codex --recovery immediate
```

Use `--project api` to select one project. Scope accepts a backend ID, kind,
provider, or backend/provider; omitting it selects all backend scopes.
The default `--recovery ramping` and automatic recovery retain the gradual
progress-based ramp. Immediate recovery also removes a matching ramp left by an
earlier clear, making repeated clears safe.

Immediate recovery preserves unrelated backend outages and recovery ramps,
GitHub and tracker holds, project breakers, human holds, configured global,
project and provider concurrency, and budget limits. It does not reset provider
usage. A renewed usage-limit response pauses dispatch normally.

The command queues work asynchronously. JSON output reports
`recovery_requested` (`ramping` or `immediate`) and `recovery_applied: pending`;
HTTP 202 means accepted, not yet applied. The orchestrator emits a structured
`backend capacity clear applied` log with `scope`, `recovery_requested`,
`recovery_applied`, and the number of outages `cleared` when it processes the
request. Other holds may still prevent dispatch after immediate recovery.

Immediate mode requires a server that supports this option. If an older server
accepts the clear but ignores the mode, the CLI reports that immediate recovery
was not acknowledged; that older server may still apply its default ramp.

### Worker CI trigger labels

`detent hub ci-trigger-label` reapplies the configured pull-request CI label for
Hub project workers, preserving host-wide staggering. The retired top-level
`ci-trigger-label` command is no longer available.

## Updates

`detent update --check` (with `--json` for machine output) reports the current
and latest versions, `update_available`, and `repair_available` for a
same-version reinstall when the running release build lacks signature evidence.
`detent update --yes` fetches the release's signed checksums, provenance and
platform archive, verifies them with the pinned minisign key, executes the
staged binary to confirm its version and full commit match the signed
provenance, and installs it atomically. When a local runner service is running,
its update owner prepares compatible worker artifacts while work is active and
replaces the controller once idle; capacity, routing, credentials and
workspaces are preserved. The updater fails closed when the signature,
provenance checksum, repository, tag, commit or required release evidence is
missing or inconsistent. Do not edit verification metadata to clear a refused
state; the signed updater can recreate it.

Enrolled runners also learn the Hub's target version on every heartbeat and
apply compatible releases the same way. Older runners keep taking work during an
update: the Hub refuses claims only from runners below its minimum supported
runner version. A failed update is reported on the next heartbeat, and a newer
release replaces an update that was refused or interrupted.

Self-update depends on how the binary was installed:

| Install source | `detent update` behavior |
| --- | --- |
| Release installer (`install.sh`, `install.ps1`) | Verifies and replaces the binary |
| Homebrew | Reports `brew upgrade digitaldrywood/tap/detent`; an enrolled runner following its Hub applies the signed release |
| `go install` | Reports the prepared source archive build path; `--from-release` switches to the release-managed binary |
| Development build without release metadata | Refused |
| Any other source (Scoop, Winget, deb/rpm, manual copy) | Refused; Detent does not overwrite a binary it cannot attribute to the release installer |

Go module archives and automatic GitHub source archives do not contain the
generated web assets. The release's `detent_<version>_source.tar.gz`, covered by
the signed checksums, contains them and the build flags, so it builds with Go
alone. The shell and PowerShell installers fall back to that archive when no
platform binary is published.

## Windows runners

Windows has two runner paths:

| Path | Agent access | Background runner |
| --- | --- | --- |
| Native Windows | Full access (`native-trusted`) only | Per-user logon Scheduled Task |
| Linux inside WSL2 | Sandbox (`sandbox`) or Full access | Linux systemd user service |

Full access lets agents use the runner user's files, credentials and network.

### Native Windows

Git for Windows (including Git Bash), the provider CLIs and the project's build
tools must be installed for, and signed in as, the Windows user who enrolls and
runs Detent, and be on that user's `PATH` at registration: the Scheduled Task
captures the enrolling session's `PATH`. Install from a non-elevated PowerShell
session:

```powershell
irm https://raw.githubusercontent.com/digitaldrywood/detent/main/install.ps1 | iex
```

The installer places `detent.exe` in `%LOCALAPPDATA%\detent\bin` and adds it to
the user `PATH`. Obtain a one-time enrollment command from **Settings > Runners >
Enroll a runner** with Full access selected, and pass the token through the
environment:

```powershell
$env:DETENT_RUNNER_ENROLLMENT_TOKEN = 'TOKEN'
detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID --name "Windows runner" --isolation-tier native-trusted --service
Remove-Item Env:DETENT_RUNNER_ENROLLMENT_TOKEN
```

Native Windows rejects the default `sandbox` tier before enrollment.
`--service` installs and starts a per-user logon Scheduled Task that runs
without elevation as the enrolling user and starts again when that user signs
in; it is not a machine service that runs before logon.

| File | Default Windows path |
| --- | --- |
| Runner configuration | `%USERPROFILE%\.config\detent-runner\global.yaml` |
| Private runner identity | `%USERPROFILE%\.config\detent-runner\identity.json` |
| Service standard output | `%USERPROFILE%\.config\detent-runner\logs\service.out.log` |
| Service standard error | `%USERPROFILE%\.config\detent-runner\logs\service.err.log` |
| Scheduled Task definition | `%USERPROFILE%\.config\detent-runner\detent.runner.task.xml` |

`--config` (an absolute path) moves the configuration; the identity, task
definition and logs live beside it. The default checkout root is
`%USERPROFILE%\detent-runner`; `--workspace-root` selects another absolute path.

An omitted `hooks.shell` uses `cmd` on Windows. `hooks.shell: bash` resolves
`bash` from `PATH`, falling back to `C:\Program Files\Git\bin\bash.exe`; a full
executable path, `pwsh` or `powershell` can also be declared. A missing declared
shell fails the hook instead of falling back to another shell.

`detent update --yes` drains active work, exits, and Task Scheduler restarts the
updated executable through the task's one-minute restart-on-failure policy. The
executable must stay at its registered path. On native Windows the Sandbox tier
and the workspace terminal are unavailable, and the executable is not
code-signed, so SmartScreen may warn.

### Linux inside WSL2

A WSL2 distribution runs the Linux Detent binary, with Git, provider CLIs and
project dependencies installed and signed in inside the distribution; Windows
installations do not carry over. `--service` requires systemd, enabled in the
distribution's `/etc/wsl.conf`:

```ini
[boot]
systemd=true
```

After `wsl.exe --shutdown` (which stops every running distribution) and reopening
the distribution, `systemctl status` confirms systemd. Install and enroll from the
Linux shell with a fresh enrollment token:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
export DETENT_RUNNER_ENROLLMENT_TOKEN='TOKEN'
detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID --name "WSL2 runner" --isolation-tier sandbox --service
unset DETENT_RUNNER_ENROLLMENT_TOKEN
```

Sandbox availability still depends on the selected backend's isolation support
and the runner's isolation probe. Keep the workspace root on the Linux
filesystem, such as `~/detent-runner`, rather than under `/mnt/c`, and keep
Windows and WSL configurations, identities and workspace roots separate. The
runner is available only while its distribution is running.

## OpenCode backend

The `opencode` backend kind runs OpenCode v2 through ACP protocol version 1 over
a private subprocess's stdin and stdout:

```yaml
agents:
  backends:
    - id: opencode
      kind: opencode
      protocol: acp
      command: opencode
      options:
        turn_timeout_ms: 1800000
        stall_timeout_ms: 300000
```

Options are `turn_timeout_ms`, `stall_timeout_ms` and `shell`. Routes select it
with an exact OpenCode `provider/model` ID; `opencode models --standalone` lists
the available IDs. The backend does not advertise a model catalog, so issue-level
model or effort overrides are unavailable, and a model-selection policy whose
backend kinds include `opencode` fails instead of selecting a model. An
unavailable model or unsupported explicit effort fails before the prompt instead
of falling back to another model.

The runner's service account authenticates with `opencode auth login`; a login
by another OS user does not authenticate the service, and `opencode auth list`
lists connections without exporting secrets. A login or catalog entry does not
prove remaining subscription capacity, and readiness reports authentication as
unknown until a real request succeeds.

Execution contract:

- Native-trusted only. Sandboxed, read-only and text-only requests fail before
  launch. OpenCode configuration, plugins and credentials are trusted runner
  inputs, not a sandbox.
- Each turn creates a new session at its exact workspace; persisted OpenCode
  sessions are not resumed, and history hydration is unsupported.
- Scoped Detent tools are served by a per-turn authenticated loopback MCP server
  whose token travels through ACP, not command-line arguments or repository
  files.
- ACP permission requests receive only `allow_once` in a trusted coding turn;
  unknown client methods are rejected.
- Cancellation, the configured turn duration, stream stall detection and
  process-group cleanup bound the process. ACP exposes no per-model-call counter,
  so Detent's turn limit cannot bound OpenCode's internal iterations.
- Launch, protocol and provider request errors use the instance-owned
  backend-capacity recovery path. A refusal because a model requires OpenCode's
  Global region setting fails the run with a message pointing to the workspace's
  Settings > Privacy > Regions in the OpenCode console; it is not treated as a
  code defect.
- End-turn token usage is provider-reported. Cache reads and writes are
  normalized into input and reasoning into output; missing counters are not
  zero consumption, and no subscription cost is inferred.

An opt-in live test exercises the installed CLI, authentication, a real model
and a scoped MCP tool in a temporary workspace, consuming provider capacity:

```sh
DETENT_OPENCODE_SMOKE_MODEL=provider/model \
  go test -timeout=150s ./internal/opencode -run '^TestOpenCodeLive$' -v
```
