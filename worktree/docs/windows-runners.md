# Windows runners

[Back to README](../README.md#get-started-on-detent-cloud)

Windows users have two supported runner paths:

| Path | Agent access | Background runner |
| --- | --- | --- |
| Native Windows | Full access (`native-trusted`) only | Logon Scheduled Task |
| Linux inside WSL2 | Sandbox (`sandbox`) or Full access | Linux systemd service |

Choose WSL2 if you want the Sandbox tier. Full access lets agents use the
runner user's files, credentials and network; use a dedicated machine for it.

## Native Windows

### Prerequisites

Install Git for Windows, including Git Bash, and the provider CLIs you plan to
use. Sign in to those CLIs as the same Windows user who will enroll and run
Detent. Install the project's build tools and dependencies for that user too.
Make Git, the provider CLIs and the build tools available on `PATH` before
registration: the Scheduled Task captures the enrolling session's `PATH`.

### Install and enroll

Run the PowerShell installer in a normal, non-elevated PowerShell session:

```powershell
irm https://raw.githubusercontent.com/digitaldrywood/detent/main/install.ps1 | iex
```

The installer places `detent.exe` in `%LOCALAPPDATA%\detent\bin` by default
and adds that directory to your user `PATH`. Open a new terminal, then check:

```powershell
detent --version
```

In Cloud, open **Settings > Providers & runners > Enroll a runner**, select
the projects and Full access, and obtain a one-time enrollment token. For a
self-hosted Hub, use its enrollment dialog and organization URL instead.
Replace the example URL and token below with the values from that dialog.
Pass the token through the environment rather than a command-line argument:

```powershell
$env:DETENT_RUNNER_ENROLLMENT_TOKEN = 'TOKEN'
detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID --name "Windows runner" --isolation-tier native-trusted --service
Remove-Item Env:DETENT_RUNNER_ENROLLMENT_TOKEN
```

`--isolation-tier native-trusted` is required on native Windows. The default
tier is `sandbox`; native Windows rejects it before enrollment.

`--service` installs and immediately starts a per-user logon Scheduled Task.
It runs without elevation as the enrolling user, and starts again when that
user signs in. Keep that user signed in for the runner to remain available;
this is not a machine service that starts before logon. Follow
[Cloud onboarding](cloud-onboarding.md) for project configuration and the rest
of the onboarding flow.

### Configuration and logs

Registration prints the configuration and identity paths. Defaults are:

| File | Default Windows path |
| --- | --- |
| Runner configuration | `%USERPROFILE%\.config\detent-runner\global.yaml` |
| Private runner identity | `%USERPROFILE%\.config\detent-runner\identity.json` |
| Service standard output | `%USERPROFILE%\.config\detent-runner\logs\service.out.log` |
| Service standard error | `%USERPROFILE%\.config\detent-runner\logs\service.err.log` |
| Scheduled Task definition | `%USERPROFILE%\.config\detent-runner\detent.runner.task.xml` |

`--config` changes the configuration path; use an absolute path. The identity,
task definition and service logs live beside it. The default checkout root is
`%USERPROFILE%\detent-runner`; set `--workspace-root` during registration to
choose another absolute path. The runner prepares isolated workspaces under that
root. Keep the identity file private.

### Hooks and updates

Use Git Bash for POSIX hook scripts by declaring the shell in the project's
`detent.yaml`:

```yaml
hooks:
  shell: bash
```

An omitted hook shell currently uses `cmd` on Windows. An explicit `bash` is
resolved from `PATH`, with `C:\Program Files\Git\bin\bash.exe` as a fallback.
For another Git installation, set `hooks.shell` to its full executable path.
You can also select `pwsh` or `powershell` for PowerShell hooks; install the
chosen shell and write the hooks in its syntax. A missing declared shell
fails the hook instead of falling back to a different shell. See
[hook configuration](config.md#operations-budgets-and-observability).

For a release install, request an update from PowerShell:

```powershell
detent update --yes
```

The running runner drains active work before applying the update and
restarting with the updated executable. For the Scheduled Task, Detent exits
after draining and Task Scheduler restarts it through the task's
restart-on-failure policy (a one-minute interval). Keep the installed executable
at its registered path so the Scheduled Task can start it on later logons. See
[runner service logs](operations.md#runner-service-logs) for troubleshooting.

### Known limits

- The Sandbox tier is unavailable natively; use WSL2 for Sandbox.
- The workspace terminal is unavailable on native Windows.
- The executable is unsigned and may trigger Windows SmartScreen.

## Linux runner inside WSL2

Use a WSL2 Linux distribution and run the Linux Detent binary inside it.
Install Git, provider CLIs and project dependencies in the distro, and sign
in there as the Linux user who will run Detent. Windows CLI installations
and sign-ins do not replace the Linux setup.

### Enable systemd

Enable systemd in the distro's `/etc/wsl.conf`, preserving any other settings:

```ini
[boot]
systemd=true
```

Close your WSL sessions, then run this in Windows PowerShell. It stops all
running WSL distributions, so finish other WSL work first:

```powershell
wsl.exe --shutdown
```

Reopen the distro and verify systemd from its Linux shell:

```sh
systemctl status
```

See Microsoft's [systemd setup for WSL](https://learn.microsoft.com/en-us/windows/wsl/systemd)
for WSL version and distro prerequisites.

### Install and enroll inside the distro

Follow the [Linux installation instructions](../README.md#install) and
[Cloud onboarding](cloud-onboarding.md) from the Linux shell. The installer is:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
```

Use a fresh enrollment token for this runner and its organization URL:

```sh
export DETENT_RUNNER_ENROLLMENT_TOKEN='TOKEN'
detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID --name "WSL2 runner" --isolation-tier sandbox --service
unset DETENT_RUNNER_ENROLLMENT_TOKEN
```

Here `--service` uses Linux systemd. Sandbox is available through the Linux
runner, subject to the selected provider backend's isolation support and the
runner's isolation probe; enabling systemd alone does not prove support.

Keep the repository and runner workspace root on the Linux filesystem, such
as `~/detent-runner`, rather than under `/mnt/c`. This also follows Microsoft's
[WSL filesystem guidance](https://learn.microsoft.com/en-us/windows/wsl/filesystems#file-storage-and-performance-across-file-systems).
Keep Windows and WSL runner configurations, identities and workspace roots
separate. The Linux runner is available only while its WSL distro is running.
