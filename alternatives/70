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
| Project operations | `promote`, `budget`, `exposure` |
| Audit | `audit` |
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
| `detent promote api --priority 1` | `{"status":"ok","project":"api","priority":1}` |
| `detent config path` | `{"path":"/path/global.yaml","rule":"--config"}` |
| `detent exposure [--project api]` | `{"findings":[{"source_project":"api","source_repository":"example-org/private-api","destination_repository":"example-org/public-project","issue_number":42,"issue_url":"https://github.com/example-org/public-project/issues/42","matched_identifier":"example-org/private-api"}],"warnings":[]}` |
| `detent skill install --target codex --dry-run` | The complete skill install result, including bundle/build stamps, target intent and status, every planned filesystem action, and any rollback actions. |
| `detent doctor` | `{"checks":[{"name":"Config resolution","status":"OK","detail":"...","hint":"..."}],"summary":{"ok":8,"warn":0,"fail":0},"result":"PASS"}` |

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

### Hub MCP

Connect an MCP client to the Hub for operator reads and actions. See
[generic MCP setup](mcp-capabilities.md#generic-client-setup) for authentication,
organization selection and the current tool catalog.

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
provenance, and installs it atomically. Stop the selected runner before applying
an offline CLI update. For an enrolled running runner, use the Hub
[`get_runner_update` and `update_apply` tools](mcp-capabilities.md#enrolled-runner-update-application)
to prepare the release and coordinate draining and restart. The updater fails closed when the signature,
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

### Bootstrap an older runner from a verified release

If an older runner refuses a Hub-requested release before it can update itself,
install the signed release archive directly. This example bootstraps Apple
silicon macOS from `v0.173.4`; change the version and archive name to the
Hub-required release and the host platform. Stop the runner before running it.
The command verifies the signed checksum file, verifies the archive checksum,
checks the staged version, and keeps a `.previous` binary for rollback. It
requires `curl`, `minisign`, `shasum`, and `tar`.

```sh
set -eu
version=0.173.4
tag="v$version"
asset="detent_${version}_darwin_arm64.tar.gz"
base="https://github.com/digitaldrywood/detent/releases/download/$tag"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/detent_${version}_checksums.txt" -o "$tmp/checksums"
curl -fsSL "$base/detent_${version}_checksums.txt.minisig" -o "$tmp/checksums.minisig"
minisign -Vm "$tmp/checksums" -x "$tmp/checksums.minisig" -P 'RWR9LKG/4dIAmbeS4Ow7XOSjxOE7GKiwQ0OPHNoViW5V0FSo4WU0vucx'
curl -fsSL "$base/$asset" -o "$tmp/$asset"
expected="$(awk -v name="$asset" '{file=$2; sub(/^[*]/, "", file); if (file == name) {print $1; exit}}' "$tmp/checksums")"
actual="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
test -n "$expected" && test "$actual" = "$expected"
mkdir "$tmp/unpacked"
tar -xzf "$tmp/$asset" -C "$tmp/unpacked"
reported_version="$("$tmp/unpacked/detent" --format pretty version | awk -F': ' '$1 == "version" {print $2; exit}')"
test "$reported_version" = "v$version"
binary="/absolute/path/to/installed/detent"
test -x "$binary" && test ! -e "$binary.previous" && test ! -e "$binary.new"
install -m 0755 "$tmp/unpacked/detent" "$binary.new"
mv "$binary" "$binary.previous"
mv "$binary.new" "$binary"
```

Restart the runner and verify it reports the new version before removing the
previous binary. The checksum signature verifies the release checksum file,
and the checksum comparison verifies the platform archive before replacing
the installed binary.

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

Use the Hub runner update tools to coordinate a running runner update, or stop
the runner task before applying `detent update --yes` and start it afterward.
The executable must stay at its registered path. On native Windows the Sandbox tier
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
