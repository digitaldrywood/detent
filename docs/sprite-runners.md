# Cloud runners on Fly Sprites

[Back to the documentation index](../README.md#documentation)

A Fly Sprite can host your Detent Cloud runner, agent sign-ins, and project
checkouts. The runner executes work inside the Sprite; the Hub owns the work
items and wakes the runner for new work. This guide takes you from a Fly account
to a completed issue with the console closed.

An organization owner or admin with runner-management access can
also ask **Luna** to add Sprites, add a runner, set up Fly, or provide more
capacity. The AI guided conversation from the Runners page uses the same intake.
Luna asks one question at a time and remembers answers already given:

1. Scope defaults to All projects, including projects created later. One Sprite pool belongs to the organization.
2. Luna reads whether the Sprites organization token is set. If it is missing,
   follow the secure connector link, save it there, and return to continue.
3. How many runners? The first-runner default is a floor and ceiling of 1/1,
   which you can accept with "yes". The floor retains minimum capacity and can
   incur Fly usage without queued work; the ceiling limits growth and potential
   concurrent usage. Provider subscriptions and usage are separate costs.
4. Review one preview of the settings and approve it in chat.

The organization pool serves every current and future project. Luna remembers bounds already supplied,
for example "2 and 2", without asking again. Pool bootstrap is optional; saved
extra steps are preserved and run before the pinned runner bootstrap. Project
setup comes from the repository's hooks.runner_setup after checkout. The runner
reports whether that hook is declared with its readiness report. If a connected
runner reports a checkout without it, Luna offers one Todo issue, "derive runner
setup from CI", for an agent to inspect CI workflows, Makefile, lockfiles and
toolchain files and land the setup script as a normal reviewed change. You do
not need to write setup commands in chat. Unknown readiness is not evidence of
a missing hook, and setup detection does not block enabling the pool.

Luna reports observed bootstrap progress in plain language and gives the next
action you need to take, such as provider sign-in inside the Sprite or Git
access. Follow the sign-in steps below inside the Sprite; never paste tokens,
provider API keys or private bootstrap credentials into chat. After a token,
billing or bootstrap failure,
correct the cause and approve Luna's scale-up retry. The existing pool lifecycle
cleans up the failed member and provisions within the saved bounds. A connected
runner still needs Git access, configured providers and approved repository
policy before it can claim work.

## 1. What you need

- A [Fly.io account](https://fly.io/app/sign-up) with Sprites billing enabled
  for the organization you will use. Use a dedicated Fly organization and set
  a spend alert.
- A Detent Cloud organization and project, with permission to enroll runners.
  Saving the Sprites token requires owner or admin access.
- Access to the project's Git repository, including permission to push the
  changes its workflow will land.
- A Codex or Claude Code account, or a Google provider account for Gemini
  through Pi. Provider subscriptions and API usage are separate from Fly costs.
- A terminal on your own machine for the Sprite CLI and a browser for sign-in.

All customer-specific values below are placeholders. Replace `SPRITE_ORG`,
`SPRITE_NAME`, `PROJECT_ID`, `REPOSITORY_URL`, and `vX.Y.Z` before running
commands. Use the same release tag for the bootstrap download and its
`--version` argument; the enrollment dialog supplies the Hub's published tag.

## 2. Install the Sprite CLI and sign in

On your own machine, install the CLI and sign in to Fly:

```sh
curl -fsSL https://sprites.dev/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
sprite --help
sprite login
SPRITE_ORG='example-org'
SPRITE_NAME='example-runner'
sprite list -o "$SPRITE_ORG"
```

Keep these two variables in your own machine’s shell for every later Sprite
command. Always pass the organization explicitly; a successful login does not
prove the CLI selected the intended default organization. If the scoped list
fails, resolve membership or organization authentication before creating a Sprite.

Complete the browser login with the Fly account that belongs to your chosen
organization. Fly's [CLI installation](https://docs.fly.io/sprites/cli/installation)
page also covers Windows and manual installation.

## 3. Create and save a Sprites organization token

Authenticate the intended Fly organization on your own machine:

```sh
sprite org auth -o "$SPRITE_ORG"
```

`sprite org auth -o <org>` only works when the Fly login is a member of that
organization. An organization name alone does not grant access. Otherwise,
have an authorized member create a token in the
[Sprites dashboard](https://sprites.dev/account) and share it privately.
You can also create a dedicated token there when CLI authentication succeeds;
Fly's [authentication guide](https://docs.fly.io/sprites/cli/authentication)
explains CLI token storage and retrieval.

The complete organization token has four slash-separated parts:

```text
org-slug/org-id/token-id/secret
```

The first part identifies the Fly organization. The other three identify the
organization and token and carry its secret. Copy the entire value, without
spaces or line breaks. A Fly login token, a token ID, or just the final secret
is not a Sprites organization token.

In the Hub, open **Settings > Integrations**, find
**Sprites organization token**, paste the value into **Organization token**, and choose **Set
token**. Confirm the status becomes **Connected to** your Fly organization
slug. Projects inherit this token; their project integration can set an override.

The Hub validates the format and makes an authenticated Sprite list request,
including for an empty organization. Its copy is encrypted and write-only.
**Remove** deletes the Hub's copy; revoke the token in Sprites to invalidate it.

If you received a dashboard token and still need to configure your local Sprite
CLI, Fly supports this command with the complete token substituted:

```sh
sprite auth setup --token 'org-slug/org-id/token-id/secret'
```

Treat that command as a credential: keep it out of shared terminal history and
logs. The Sprites token authorizes Fly operations; the separate enrollment
token in the next step registers a runner with Detent.

## 4. Enroll a runner in the Hub

Open **Settings > Runners > Enroll a runner**. Under **Where will it run?**,
select **A Fly Sprite**. New Sprite runners use **Full access**
(`native-trusted`), which lets agents use the VM's files, credentials and
network. The enrollment access choice is passed to
`detent hub runner register --isolation-tier`.

An owner or admin can approve switching an individual Sprite runner to Sandbox
to request its isolation probe. Sandbox work remains unavailable until the probe
reports sandbox support for every configured agent backend. Full access Sprites
skip this optional sandbox probe. Sprite processes inherit an ambient Linux
capability set that makes the Codex sandbox refuse to start, so
Detent v0.117.42 and later start sandboxed agent processes with inheritable
and ambient capabilities dropped. Older runners fail the probe and cannot run
Sandbox work; update the runner and wait for its next heartbeat, or approve
switching it to Full access. Sandbox limits agent writes to the workspace and
outbound network to the sandbox domain allowlist; it does not hide files
elsewhere on the VM from agent reads.

For **Add runner > AI assisted**, Luna explains Full access and includes
`isolation_tier: native-trusted` in new approved Sprite pool settings. The saved
choice applies to newly provisioned pool members. Existing runners and pools
configured for sandbox retain their stored tier and report why it is unavailable
until their probe passes. Sprite pools cannot select Sandbox.
Changing an existing runner uses a separate `set_runner_tier` preview: an
organization owner or admin must approve it, even when ordinary chat
confirmations are off. No re-enrollment is needed. Ask Luna why a runner is not
working to read its tier and problems. Projects requiring sandbox cannot use
full-access runners.

1. Enter the Sprite name as the runner's **Name**. Use lowercase letters,
   numbers, and hyphens, at most 63 characters, starting and ending with a
   letter or number. Use this same name when creating the Sprite.
2. Start with **Concurrency** set to `1`.
3. Select only the **Projects** this runner will serve. Resolve any **No
   Sprites token is set** warning through the integration settings above.
4. Choose **Create command** and keep the dialog open for the setup commands.

The generated `detent hub runner register ...` command contains a single-use
enrollment token valid for up to 15 minutes. Copy the full command when the
bootstrap asks for it. If it expires before registration, create a fresh
command. Closing the dialog clears its displayed command.

## Repository project setup

A project declares its tool installation once in its `detent.yaml`, for example:

```yaml
hooks:
  runner_setup: scripts/runner-setup.sh
  shell: bash
  timeout_ms: 900000
```

The script is committed with the repository. It runs non-interactively from the
source checkout with the runner's permissions and inherited environment;
exports inside it do not change later agent environments. Because it can run
again on any runner, it works best when idempotent (for example, skipping a CLI
already installed at the required version) and contains no secrets.

Both pool members and manually enrolled Sprites use the same runner owner.
It runs setup when preparing the project, records a successful content hash in
that Sprite's local store, and runs again when the script content, configured
shell or checkout path changes, or when the checkout is recreated. Later
attempts and runner restarts reuse that hash. Each Sprite has its own record, so
every member installs the tools automatically. The hash covers only the script,
shell and checkout path: changing a file the script reads, such as Makefile tool
pins or a lockfile, does not by itself rerun setup on existing runners.

Before enrollment, the bootstrap records the Sprite's setup revision in
`~/.local/lib/detent-sprite-runner/setup-manifest.json`: platform, bootstrap
release, pinned toolchain, and the allowlisted native caches (`go-build`,
`go-mod`, `npm`). It never contains provider logins, GitHub credentials, the
runner identity, or job data. Tools found outside the managed set in
`~/.local/bin` or the runner's npm prefix are reported as customizations the
setup cannot reproduce. The manifest is
install output only: the runner never reads it, so jobs do not depend on it.
Native caches are optional and never copied between Sprites. A missing cache is
rebuilt by its toolchain on demand. The bootstrap never deletes cache contents:
a default npm cache that lost its marker is repaired with `npm cache verify`,
and a Go cache without its marker is reported because Go verifies entries and
rebuilds invalid ones on use. Explicit operator cache overrides stay unmanaged.
After resolving symlinks and parent segments, the bootstrap refuses a cache
path that overlaps provider logins (including `GH_CONFIG_DIR`, `CODEX_HOME`,
`CLAUDE_CONFIG_DIR` and `DOCKER_CONFIG`), credentials, the runner
configuration or identity file, or contains the requested or configured workspace root, and leaves a cache
inside the workspace root unmanaged. The manifest records exact APT package revisions and the
distribution, so a fresh Sprite with different repository candidates does not
match the revision.

To reproduce a revision on a fresh Sprite without the original, pass its
manifest: `bash sprite-runner-bootstrap.sh --version vX.Y.Z --setup-manifest
setup-manifest.json`. The bootstrap stops before enrollment if the platform,
release, toolchain, or cache allowlist differ, or if either side has
customizations it cannot reproduce. Each new Sprite still signs in once in its
own console (for example `codex login --device-auth`, `gh auth login` and
`gh auth setup-git`); provider logins are never copied.

A failure prevents that project's claims and dispatch on that Sprite until
setup succeeds. The runner reports an instance failure in its logs and the
project's local-check API report; no issue failure allowance is consumed.
Other projects and Sprites can continue. The existing project polling retries
the failed setup. See [project setup configuration](config.md#workspace-deliverable-and-worker-placement)
for hash, workflow-ref, timeout, and path semantics.

## 5. Create the Sprite and bootstrap the runner

On your own machine, use the Fly organization whose token you saved in the Hub:

```sh
sprite create -o "$SPRITE_ORG" "$SPRITE_NAME"
sprite console -o "$SPRITE_ORG" -s "$SPRITE_NAME"
```

If creation already opens a console, continue there. The following commands
run **inside the Sprite**, from its home directory. Substitute the published
release tag shown in the enrollment dialog:

```sh
cd "$HOME" &&
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/vX.Y.Z/scripts/sprite-runner-bootstrap.sh -o sprite-runner-bootstrap.sh &&
bash sprite-runner-bootstrap.sh --version vX.Y.Z
```

At the hidden prompt, paste the Hub's complete enrollment command and press
Enter. Input does not echo. Paste it at that prompt rather than running it as
a shell command. The bootstrap parses it without shell expansion and passes
its token through the environment to Detent. Empty input is only for a runner
already registered on this same Sprite. If installation fails before enrollment,
rerun the printed resume command and paste a fresh Hub command if the original
token expired. Do not run the register command separately, delete the identity,
or copy an identity from another Sprite.

On failure the bootstrap prints the last completed step and a quoted resume
command without the enrollment token. A tool install, successful login, service
creation, or installation checkpoint alone does not mean the runner is ready.

The script installs the standard runner toolset below and checksum-verified Detent. It
registers the runner and starts a Sprite Service named `detent-runner`.
This Service restarts after a cold wake; a host `--service` flag in the pasted
command is replaced by the Sprite Service.

The bootstrap fills missing `global.io.degraded_max_concurrent_agents` and
`global.cpu.degraded_max_concurrent_agents` settings with `1`, preserving
operator values and the rest of the configuration. Sprite I/O pressure can
exceed the default threshold even while idle; this floor lets one agent
proceed under I/O or CPU pressure. Project, pool, provider, and other capacity
limits still apply. The printed summary shows the configured floors. See
[host pressure configuration](config.md).

### Resource profile

Sprites have no machine-size option. `sprite create` and `POST /v1/sprites`
take only a name; a Sprite has 8 vCPUs and platform-managed memory with no
swap. The home directory, including the runner store, identity and
configuration, lives on the Sprite's persistent network block device. `/tmp`
is a separate 20 GB local disk. Every block device uses the `none` I/O
scheduler, so `ionice` classes and cgroup `io.weight` have no effect inside a
Sprite.

One runner slot does not bound the parallelism inside a job. A single nested
`make` gate with explicit `-j` jobs, Go builds, linters and JavaScript test
runners can saturate the CPUs and the block device, delay the runner's
heartbeat and provider probes past their deadlines, and lose the claim's
lease. The bootstrap therefore declares this profile in the Service launcher
it writes on every run:

| Control | Owner | Effect |
| --- | --- | --- |
| `MAKEFLAGS=-l<CPUs/4>`, at least 1 (`-l2` on 8 vCPUs) | Service launcher | GNU Make starts no further parallel job while the load average is above the limit, including recipes with an explicit `-j`. Linux counts tasks waiting on disk in the load average, so the limit also spaces out I/O bursts. Every target still runs. |
| Go build budget | Existing `global.cpu.go_build_budget` | Go compile, link and vet processes in a Go module workspace queue for host-wide slots, with `-p` and `GOMAXPROCS` at a quarter of the budget. |
| Lower worker CPU priority | Existing worker process owner | Agent processes and everything they start run five nice levels below the runner, so heartbeats, lease renewal and provider probes win CPU contention. |
| Local disk for checkouts and caches | Bootstrap default workspace root and native cache layout | When `/tmp` is its own ext4, XFS or Btrfs mount, new registrations use `/tmp/detent-runner/work` as the workspace root. Native cache directories that are still at their default location and empty (`~/.cache/go-build`, Detent's managed `~/.cache/detent`, `~/.cache/golangci-lint`, `~/go/pkg/mod` and `~/.npm`) become links into `/tmp/detent-runner/cache`. |

Gate builds and tests write heavily to checkouts, temporary directories and
caches. On the network block device that traffic starves the runner's probes
and can make the device disconnect, which restarts every process in the
Sprite. On the local disk it does not. The runner store, identity,
configuration, provider sign-ins, installed tools and Playwright's Chromium
stay in the home directory.

The runner sets no cache variables: tools keep their native default paths, and
only the directories behind them move. Operator cache settings, whether from
the environment, `go env -w` or `.npmrc`, are left alone, and a native cache
that already has content stays where it is. An explicit `--workspace-root` and
an operator-chosen `workspace_root` in the configuration are kept. A rerun
moves a configuration still on the old default, `~/detent-runner`, to the
local disk; checkouts left at the old path can be deleted.

Checkouts and caches are disposable. The Service starts from the home
directory, and its launcher recreates the local checkout and cache directories
before starting the runner. The runner clones a missing checkout,
`hooks.runner_setup` runs again for a moved or recreated checkout, and missing
caches are rebuilt at the cost of a slower next build. `/tmp` keeps its
contents while the Sprite is idle. The 20 GB local disk fits only a few
checkouts with their caches, so keep **Concurrency** at `1`.

The bootstrap prints the profile in its closing summary. It rewrites the
launcher on every run, so hand edits to it do not survive an update; change
the bootstrap instead.

### Standard runner toolset

Every runner receives the standard tools during bootstrap, so agents can
use them without installing dependencies during a turn. Installed tools and
browser binaries stay on the Sprite's disk across pauses and runner updates.
The bootstrap prints their versions and verifies a Chromium headless launch
and screenshot of a local page before enrolling or starting the runner.

| Tool | Pin | Purpose |
| --- | --- | --- |
| Go | Toolchain in the release's `go.mod` | Build and test Go projects with the required compiler. |
| GitHub CLI (`gh`) | 2.101.0 | GitHub authentication and repository workflows; includes `--attach` for issue and PR comments. |
| Codex | 0.160.0 | Replace the image's broken preinstalled Codex with the npm CLI. |
| Claude Code | 2.1.289 | Run the Claude backend using the npm CLI. |
| Playwright | 1.63.0, with its matching Chromium | Verify web behavior headlessly and take screenshots. |
| ripgrep (`rg`) | 15.2.0 | Search source text quickly. |
| fd | 10.3.0 | Find source files quickly. |
| PostgreSQL client (`psql`) | APT candidate for major 18, or 17 when 18 is unavailable | Query PostgreSQL databases for development and diagnostics. |
| Docker | APT candidate for `docker.io`, when supported | Run container-based project dependencies. |

Go, Detent, GitHub CLI and ripgrep downloads are checked against published
SHA-256 manifests. fd archives are checked against the release's published
SHA-256 digests pinned in the script. npm verifies package integrity, and APT
verifies the signed Ubuntu package indexes and package checksums.
Before downloading Go, agent CLIs or Chromium, the bootstrap refreshes APT
indexes and checks compatible PostgreSQL and, when supported, Docker candidates.
A missing package fails here rather than after expensive downloads. It uses the
image’s signed repositories, never an Ubuntu release suffix copied from another
image. Docker selects the candidate from those same configured repositories only
when the Sprite supports its namespace and cgroup requirements. It installs
that exact revision and reports a repository compatibility error when no
candidate is available.
PostgreSQL uses the configured distribution repositories: Resolute supplies
major 18, while images supplying only major 17 keep that major. The selected
candidate revision is passed explicitly to APT. No external repository or
server package is installed. Missing compatible candidates fail with a
repository compatibility error. The bootstrap locates and verifies the
package's executable before updating its local `psql` link, and reinstalls
the client when a partial installation left its executable missing.
The pinned `yaml` Node package (2.8.3) merges the pressure defaults while
preserving existing values and YAML comments.

Playwright installs Chromium and its Ubuntu system dependencies when the
headless screenshot probe fails. Its native Linux browser cache is
`~/.cache/ms-playwright`; the bootstrap preserves explicit browser-cache
overrides. Only Chromium is installed, including its headless shell. The
Playwright CLI is on PATH, so `npx playwright` can use the installed version
from a checkout without a project-specific Playwright dependency. A checkout
with its own Playwright version may require that version's browsers.

Docker installation first checks root mount, network and PID namespaces and
writable cgroups. If those capabilities are unavailable, the closing summary
explains why Docker was skipped. When installed and no accessible daemon is
running, a Sprite Service named `detent-docker` starts the daemon with persistent
data under `~/.local/share/detent-runner/docker` and a socket accessible to the
Sprite user's primary group. Docker runtime support still needs verification
on the target Sprite with `docker info` and a project container.

Re-running the bootstrap with empty input upgrades tools to these pins and
skips downloads and package installs for current tools. It repeats the local
Chromium screenshot probe, reuses the runner identity, restarts the runner
service and creates a checkpoint. Provider credentials and checkouts remain
on disk. These are bootstrap operations; agents do not repeat the installs.

Default locations are:

| Item | Location inside the Sprite |
| --- | --- |
| Runner configuration | `~/.config/detent-runner/global.yaml` |
| Private runner identity | `~/.config/detent-runner/identity.json` |
| Project checkouts | `/tmp/detent-runner/work/PROJECT_ID` on the local disk, otherwise `~/detent-runner/PROJECT_ID` |
| Native build caches | Default paths; on the local disk they link into `/tmp/detent-runner/cache` |
| Service launcher | `~/.local/lib/detent-sprite-runner/start` |

Keep the paths printed by registration if you selected custom paths. The
script takes an initial checkpoint and prints its ID, but provider sign-in,
Git access, and checkout preparation still need the following steps. Use one
runner per Sprite and keep its identity on that Sprite.

## 6. Sign in to your agents

Inside the Sprite, put the installed binaries on your console's path:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Detent supports these backend kinds:

| Agent | Detent backend kind | Sign-in |
| --- | --- | --- |
| Codex | `codex` | `codex login --device-auth` |
| Claude Code | `claude_code` | `claude auth login` |
| Pi, for Gemini | `pi_agent` | Install Pi and authenticate its Google provider |

Run the login for each provider your project's routes use. Follow the device
or browser link on your own machine, then verify inside the Sprite:

```sh
codex login status
claude auth status
```

For Gemini, use **Pi** with `kind: pi_agent` and `provider: google`. The bootstrap
does not install Pi. Follow the [Pi backend guide](pi-agent.md) for the required
RPC protocol, provider authentication, and route/model selection. A backend
entry in the runner's existing `agents.backends` list looks like:

```yaml
agents:
  backends:
    - id: pi-google
      kind: pi_agent
      protocol: rpc
      command: pi
      provider: google
```

Merge the entry into your configuration and select it through the existing
agent routes with an exact model ID from your Pi catalog. Pi requires the
operator to select **native-trusted isolation**. It cannot run sandboxed or
read-only roles; keep validation on another configured backend.

Detent also has an `opencode` backend kind; see
[OpenCode backend](cli.md#opencode-backend). The bootstrap does not install
OpenCode. A CLI being available in a Sprite image does not make it a Detent
backend; the standalone Gemini CLI is not one.

## 7. Prepare Git access and the project checkout

Inside the Sprite, for GitHub-hosted repositories you can configure Git access
with the installed GitHub CLI:

```sh
gh auth login --hostname github.com --git-protocol https --web &&
gh auth setup-git &&
gh auth status --hostname github.com
```

Alternatively, use the project's existing SSH or HTTPS Git credentials. The
runner needs clone/fetch access and the push access required by the project's
landing workflow. Configure the Git author and any signing setup that workflow
requires:

```sh
git config --global user.name 'YOUR_NAME'
git config --global user.email 'YOUR_EMAIL'
git ls-remote REPOSITORY_URL HEAD
```

Use the runner-managed checkout; do not make a second clone for dependency
installation. On a fresh Sprite the runner clones each allowed repository into
`workspace_root/PROJECT_ID`. The bootstrap prints the absolute root and exact
paths for the project IDs stored in its enrollment. For organization-wide
runners, use the project’s exact `prj_` ID from the Hub after it is granted.
The runner can retain an existing matching checkout under that root; if one is
reported, confirm that selected path through the runner’s existing project
local-check report before using it. A clone elsewhere is not evidence of the
configured checkout.

Inside the Sprite, replace the project ID below and set `ROOT` to the managed
checkout root the bootstrap printed: `/tmp/detent-runner/work` on a Sprite with
a local disk, otherwise `~/detent-runner`, or your custom root.
Wait until the runner has cloned the checkout after Git authentication:

```sh
PROJECT_ID='prj_example'
ROOT='/tmp/detent-runner/work'
CHECKOUT="$ROOT/$PROJECT_ID"
printf 'Managed checkout: %s\n' "$CHECKOUT" &&
test -d "$CHECKOUT/.git" &&
git -C "$CHECKOUT" rev-parse --show-toplevel &&
git -C "$CHECKOUT" remote get-url origin
```

Check the printed origin against your selected project. If the checkout is
missing or the origin is wrong, inspect runner setup logs and Hub project grants;
do not run dependencies from your home directory or create a replacement clone.
Repository-declared `hooks.runner_setup` is the preferred dependency owner.
For a pnpm repository, first select the repository’s Node version and ensure
its declared Corepack is available. This guarded example refuses a repository
without a pnpm `packageManager` pin:

```sh
cd "$CHECKOUT" &&
test -d .git &&
node -e 'if (!require("./package.json").packageManager?.startsWith("pnpm@")) process.exit(1)' &&
corepack pnpm --version &&
corepack pnpm install --frozen-lockfile &&
printf 'pnpm dependencies completed (exit 0)\n'
```

Do not install an unpinned global pnpm. For a Go module at the checkout root:

```sh
cd "$CHECKOUT" && test -f go.mod && go mod download &&
printf 'Go downloads completed (exit 0)\n'
```

For a nested module, use its path declared in the repository instead of guessing
an application directory. Go downloads may be silent; the chained success message
confirms exit status 0. A failed directory change stops the entire chain.

The runner reads `detent.yaml` and `WORKFLOW.md` from the repository's current
default branch, including `hooks.runner_setup` and the agent routes; see
[Cloud onboarding](cloud-onboarding.md#repository-configuration-and-policy).
Each enrolled project has its own checkout and configuration.

When the runner reports a pending repository policy, the project's **Settings**
show the observed and approved policy. An organization owner or admin approves
the exact observed policy through the `approve_project_policy` MCP tool or
`detent hub policy approve`. Configuration reports retain policy provenance; they
are not a recurring approval gate on claims or publication.

## 8. Restart and take a checkpoint

After sign-in, checkout, and configuration changes, rerun the same pinned
bootstrap **inside the Sprite** with empty input:

```sh
cd "$HOME" &&
bash sprite-runner-bootstrap.sh --version vX.Y.Z < /dev/null &&
sprite-env services get detent-runner
```

Empty input reuses the registered identity and restarts the existing Service;
it does not redeem another enrollment token. The bootstrap preserves existing
configuration values, fills missing pressure floors, and takes a checkpoint.
Verify the Service reports a running process, not just that a definition exists;
inspect the runner log if it exits. The bootstrap checkpoint is an installation
checkpoint. Save the ready checkpoint only after the real task below completes. Restoring with
`sprite-env checkpoints restore CHECKPOINT_ID` rewinds the same Sprite's files,
including credentials; it does not renew credentials that have since expired.
This checkpoint holds the runner identity and logins, so it is for recovering
this Sprite only, never a reusable setup for another Sprite.

Check **Settings > Runners** for its connection, provider, and project readiness.
Resolve any remaining policy or authentication requirements before testing wake.

## 9. Verify the wake loop with no console open

1. Exit the Sprite console with `exit`. Close any remaining exec, console,
   or port-forwarding sessions. Leave the Sprite idle until it is **cold**;
   it may first become **warm**. Check status in the Sprites dashboard or,
   from your own machine, with `sprite list -o "$SPRITE_ORG"`.
2. In your Detent Cloud project, create a small issue in a lane that is
   dispatchable in that project's approved workflow, such as **Todo**.
3. Keep the console closed. Confirm in the Hub that the runner reconnects,
   claims the issue, and publishes a Change Request, and that the change lands
   through the project's workflow.
4. Reopen the same Sprite console from your own machine with
   `sprite console -o "$SPRITE_ORG" -s "$SPRITE_NAME"`. Inside the Sprite, run
   `sprite-env checkpoints create --comment 'runner verified: task published'`
   and retain the returned checkpoint ID. This is the final ready checkpoint;
   a failed checkpoint remains incomplete even when the task succeeded. Exit again.
5. With no further work or open sessions, confirm the Sprite pauses again
   (warm, then eventually cold).

The Hub uses the project's token override, or its inherited organization token, to start the cold runner's
`detent-runner` Service. Detent holds a Sprite task during active work and
releases it afterward. An open console would wake the Sprite itself and would
not verify this loop. A connected runner alone is not proof that it can claim
and land work. Fly explains Service and task behavior in
[Keeping a Sprite running](https://docs.fly.io/sprites/keeping-sprites-running).

## 10. Updating, costs, and troubleshooting

### Updating

Inside the Sprite, take a checkpoint before updating. Download the bootstrap
from the chosen new release tag and run it with the same tag and empty input:

```sh
cd "$HOME" &&
sprite-env checkpoints create --comment 'before runner update' &&
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/vX.Y.Z/scripts/sprite-runner-bootstrap.sh -o sprite-runner-bootstrap.sh &&
bash sprite-runner-bootstrap.sh --version vX.Y.Z < /dev/null &&
detent --version &&
sprite-env services get detent-runner
```

The script reuses enrollment, preserves configuration and checkouts, installs
the pinned Detent release, refreshes the agent CLIs, and restarts the Service.
Verify provider sign-in and repeat the console-closed wake test, then save a
new ready checkpoint.

### Costs

Sprites bills consumed resources, rather than a fixed VM size. Fly's published
unit rates checked for this guide are:

| Consumed resource | Rate in USD |
| --- | ---: |
| CPU time | $0.03825 per CPU-hour |
| Memory | $0.021875 per GB-hour |
| Hot storage | $0.000683 per GB-hour |
| Cold storage | $0.000027 per GB-hour |

Compute billing stops while paused; persisted storage remains billable. Agent
provider costs are separate. Plan allowances, concurrency limits, and rates
can change; [Fly's current Sprites pricing](https://fly.io/sprites/#pricing) is
authoritative. Detent does not read Fly invoices or per-Sprite billing; measured
or estimated Sprite costs can be imported into the Hub's monthly usage ledger,
as described in [Sprite infrastructure usage](runner-usage.md#sprite-infrastructure-usage).

### Troubleshooting

For interrupted dependency installs, re-enter the verified managed checkout and
rerun its pinned install command; preserve lockfiles, authentication and runner
identity. For EROFS/EIO, check the actual filesystem before retrying, inside the
Sprite:

```sh
cd "$CHECKOUT" && df -h . && df -i . &&
probe=$(mktemp .detent-write-probe.XXXXXX) && rm -- "$probe"
```

If storage is full, reclaim only data you own. If writes still fail or the
console disconnects, stop dependency installation and use Fly’s infrastructure
support. Ample space does not prove
that an earlier EROFS/EIO cause is resolved. After a successful write check,
a pnpm install retry with lower parallelism was observed to succeed:

```sh
cd "$CHECKOUT" && test -d .git &&
node -e 'if (!require("./package.json").packageManager?.startsWith("pnpm@")) process.exit(1)' &&
corepack pnpm install --frozen-lockfile --network-concurrency=4 --child-concurrency=2 &&
printf 'pnpm retry completed (exit 0)\n'
```

This is a recovery option, not a required setting or a diagnosis of the underlying
storage failure.

| Symptom | What to check |
| --- | --- |
| Hub rejects the Sprites token | Use the complete `org-slug/org-id/token-id/secret` value with no whitespace. Confirm it is active and belongs to the same Fly organization as the Sprite. The Hub must reach the Sprites API and have its secret keys configured; ask the Hub operator if a valid token still cannot be saved. |
| `org not found` from the Sprite CLI | Check the Fly organization slug and the account used by `sprite login`. `sprite org auth -o <org>` needs membership; otherwise obtain a dashboard token from an authorized member and configure it with `sprite auth setup`. |
| Work waits while the Sprite is cold | Check the project's Sprites integration, the runner's selected project scope and active identity, the exact Sprite name/organization, and that the issue is in a dispatchable lane. Confirm the `detent-runner` Service exists. If it wakes but does not claim work, check repository policy approval, checkout/dependencies, provider authentication/routes, capacity, and both degraded-capacity fields. Manually opening a console helps diagnose but does not prove automatic wake. |
| Hub `401` after a long stop | Runner credentials last 24 hours and normally renew while active. If the credential expired while stopped, create a fresh Hub enrollment command for the same name, projects, and capacity, then rerun the bootstrap and paste it at the hidden prompt. Registration reuses the identity and configuration on that Sprite. Empty input alone cannot re-enroll an expired credential. Check Hub revocation/grants if re-enrollment is refused. |
| Runner starts then fails, or an issue does not progress | Inspect the Service with `sprite-env services get detent-runner` and read `~/.config/detent-runner/detent.log`. With a custom configuration path, the default log sits beside that configuration unless the runtime log/database path is overridden. |

Reopen the console for diagnosis, read the log, and exit again before retesting:

```sh
tail -n 100 "$HOME/.config/detent-runner/detent.log"
```

If bootstrap fails only at checkpoint creation, the runner may already be
registered and its Service installed. Inspect those before retrying; preserve
the existing configuration and identity.
