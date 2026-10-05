# Cloud runners on Fly Sprites

[Back to the documentation index](../README.md#documentation)

A Fly Sprite can host your Detent Cloud runner, agent sign-ins, and project
checkouts. The runner executes work inside the Sprite; the Hub owns the work
items and wakes the runner for new work. This guide takes you from a Fly account
to a completed issue with the console closed.

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

All customer-specific values below are placeholders. Replace `ORG_SLUG`,
`SPRITE_NAME`, `PROJECT_NAME`, `REPOSITORY_URL`, and `vX.Y.Z` before running
commands. Use the same release tag for the bootstrap download and its
`--version` argument; the enrollment dialog supplies the Hub's published tag.

## 2. Install the Sprite CLI and sign in

On your own machine, install the CLI and sign in to Fly:

```sh
curl -fsSL https://sprites.dev/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
sprite --help
sprite login
```

Complete the browser login with the Fly account that belongs to your chosen
organization. Fly's [CLI installation](https://docs.fly.io/sprites/cli/installation)
page also covers Windows and manual installation.

## 3. Create and save a Sprites organization token

Authenticate the intended Fly organization on your own machine:

```sh
sprite org auth -o ORG_SLUG
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

In the Hub, open **Settings > Integrations**, select the project, find
**Sprites**, paste the value into **Organization token**, and choose **Set
token**. Confirm the status becomes **Connected to** your Fly organization
slug. Repeat for every project this Sprite will serve: the saved integration
is scoped to a project.

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
select **A Fly Sprite**.

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

## 5. Create the Sprite and bootstrap the runner

On your own machine, use the Fly organization whose token you saved in the Hub:

```sh
sprite create -o ORG_SLUG SPRITE_NAME
sprite console -o ORG_SLUG -s SPRITE_NAME
```

If creation already opens a console, continue there. The following commands
run **inside the Sprite**, from its home directory. Substitute the published
release tag shown in the enrollment dialog:

```sh
cd "$HOME"
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/vX.Y.Z/scripts/sprite-runner-bootstrap.sh -o sprite-runner-bootstrap.sh
bash sprite-runner-bootstrap.sh --version vX.Y.Z
```

At the hidden prompt, paste the Hub's complete enrollment command and press
Enter. Input does not echo. Paste it at that prompt rather than running it as
a shell command. The bootstrap parses it without shell expansion and passes
its token through the environment to Detent.

The script installs the Go toolchain required by that release, GitHub CLI,
the npm builds of Codex and Claude Code, and checksum-verified Detent. It
registers the runner and starts a Sprite Service named `detent-runner`.
This Service restarts after a cold wake; a host `--service` flag in the pasted
command is replaced by the Sprite Service.

Default locations are:

| Item | Location inside the Sprite |
| --- | --- |
| Runner configuration | `~/.config/detent-runner/global.yaml` |
| Private runner identity | `~/.config/detent-runner/identity.json` |
| Project checkouts | `~/detent-runner/PROJECT_NAME` |
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

**OpenCode is not a Detent backend.** A CLI being available in a Sprite image
does not make it a supported Detent backend, including the standalone Gemini
CLI.

## 7. Prepare Git access and the project checkout

Inside the Sprite, for GitHub-hosted repositories you can configure Git access
with the installed GitHub CLI:

```sh
gh auth login
gh auth setup-git
```

Alternatively, use the project's existing SSH or HTTPS Git credentials. The
runner needs clone/fetch access and the push access required by the project's
landing workflow. Configure the Git author and any signing setup that workflow
requires:

```sh
git config --global user.name 'YOUR_NAME'
git config --global user.email 'YOUR_EMAIL'
git clone REPOSITORY_URL "$HOME/detent-runner/PROJECT_NAME"
```

Use the exact checkout directory printed by registration, not an inferred
repository name. Each checkout must contain the project's `detent.yaml` and
`WORKFLOW.md`. Install its build and test dependencies and configure the agent
routes it uses; see [Cloud onboarding](cloud-onboarding.md#repository-configuration-and-policy).
Repeat for every enrolled project.

When the runner reports a pending repository policy, open Hub **Settings >
Projects**, select the project, and open its **Settings**. Review the reported
policy and use **Approve updated policy** if offered. An owner/admin with the
project's write access must approve it before dispatch.

## 8. Allow one agent under Sprite resource pressure

Edit the runner's `~/.config/detent-runner/global.yaml`. Merge these fields
into its existing `global` mapping, preserving the rest of the enrollment
configuration:

```yaml
global:
  io:
    degraded_max_concurrent_agents: 1
  cpu:
    degraded_max_concurrent_agents: 1
```

A Sprite's idle I/O pressure sits above Detent's default threshold. The
default degraded capacity is `0`, so pressure can otherwise hold all dispatch
even when no agents are running. Set both
`global.io.degraded_max_concurrent_agents: 1` and
`global.cpu.degraded_max_concurrent_agents: 1` so one worker can proceed under
either signal. This is a host-wide floor; project, pool, provider, and other
capacity limits still apply. See [host pressure configuration](config.md).

## 9. Restart and take a checkpoint

After sign-in, checkout, and configuration changes, rerun the same pinned
bootstrap **inside the Sprite** with empty input:

```sh
cd "$HOME"
bash sprite-runner-bootstrap.sh --version vX.Y.Z < /dev/null
sprite-env services get detent-runner
sprite-env checkpoints create --comment 'runner ready: agents and checkouts configured'
```

Empty input reuses the registered identity and restarts the existing Service;
it does not redeem another enrollment token or overwrite `global.yaml`. The
bootstrap also takes a checkpoint. Save the ready checkpoint ID so your
baseline includes the manual setup. Restoring with
`sprite-env checkpoints restore CHECKPOINT_ID` rewinds the same Sprite's files,
including credentials; it does not renew credentials that have since expired.

Check **Settings > Runners** for its connection, provider, and project readiness.
Resolve any remaining policy or authentication requirements before testing wake.

## 10. Verify the wake loop with no console open

1. Exit the Sprite console with `exit`. Close any remaining exec, console,
   or port-forwarding sessions. Leave the Sprite idle until it is **cold**;
   it may first become **warm**. Check status in the Sprites dashboard or,
   from your own machine, with `sprite list -o ORG_SLUG`.
2. In your Detent Cloud project, file a small, focused **Todo** issue, such as
   a requested documentation correction. The lane must be dispatchable in
   that project's approved workflow.
3. Keep the console closed. Confirm in the Hub that the runner reconnects,
   claims the issue, and publishes a Change Request. Complete any review or
   check steps required by your project, then confirm the change lands and
   the issue reaches **Done**.
4. With no further work or open sessions, confirm the Sprite pauses again
   (warm, then eventually cold).

The Hub uses the selected project's Sprites token to start the cold runner's
`detent-runner` Service. Detent holds a Sprite task during active work and
releases it afterward. An open console would wake the Sprite itself and would
not verify this loop. A connected runner alone is not proof that it can claim
and land work. Fly explains Service and task behavior in
[Keeping a Sprite running](https://docs.fly.io/sprites/keeping-sprites-running).

## 11. Updating, costs, and troubleshooting

### Updating

Inside the Sprite, take a checkpoint before updating. Download the bootstrap
from the chosen new release tag and run it with the same tag and empty input:

```sh
cd "$HOME"
sprite-env checkpoints create --comment 'before runner update'
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/vX.Y.Z/scripts/sprite-runner-bootstrap.sh -o sprite-runner-bootstrap.sh
bash sprite-runner-bootstrap.sh --version vX.Y.Z < /dev/null
detent --version
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
can change: check [Fly's current Sprites pricing](https://fly.io/sprites/#pricing)
and your actual usage before choosing a plan. Keep a spend alert on the Fly
organization.

### Troubleshooting

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
