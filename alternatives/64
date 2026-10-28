# Project onboarding

Detent Cloud starts at `https://cloud.detent.build`: sign in, create or join an organization, then open a project shared with you. Organization administrators create projects; collaboration and runner management are separate explicit grants. Retrying project creation with the same name and the creator's write grant resumes that project, and infrastructure errors during setup do not require another organization or host identity.

## Default workflow

A new hosted project starts with `Triage`, `Backlog`, `Todo`, `In Progress`, `Blocked`, `Human Review`, `Merging`, `Done` and `Cancelled`. `Todo`, `In Progress` and `Merging` dispatch work. `Triage` (linked GitHub intake and reporter filings), `Backlog`, `Blocked` and `Human Review` neither dispatch nor end it; `Done` and `Cancelled` are terminal.

By default `review.human` is false, so nobody has to review a run. A run that committed a change publishes a version, and once the configured review, validation and audit stages accept it the runner's orchestrator moves it to `Merging`, where the runner that holds the project lands the head on the base branch with plain git and the Hub finishes the issue in `Done`. A run that committed nothing moves to `Done`.

`Human Review` is where a Change Request waits for a person only when the project sets `review.human: true`; a person approves the change, which moves it to `Merging`, or requests changes, which sends it back to `In Progress`. `Human Review` is the default `auto_promote.source_state`. With the default `false`, an unaccepted version or refused landing returns to a dispatchable lane through the retry owner. `Blocked` holds only work waiting on an explicit human question or permission request (a typed `human_action`) or an operator stop.

Hub migrations 36, 38, 39 and 41 move projects whose workflow is exactly an earlier template onto this one. A customized workflow is left unchanged; it needs a review lane matching `auto_promote.source_state` when `review.human` is true, and a dispatchable `Merging` lane reachable from `In Progress` for accepted changes. Hub migration 40 gives every project with an approved repository policy a default review policy; approving the policy again seeds `require_review` from `review.human`.

The workflow can also be supplied as `states` to `PUT /api/v2/organizations/{organization}/projects/{project}/onboarding/integration` (or `/integration`) with the current `revision` as `expected_revision`, a fresh `idempotency_key`, and the current `intake`, `projection` and `repository_enabled` values; omitting `states` keeps the current workflow. Hosted project creation also accepts optional `states`. A stale revision, or removing a state used by an active or archived item, is refused atomically. The first configured state is the default lane in native issue creation forms.

## Runner access through Luna

In **Add runner > AI assisted**, an own-machine request such as “add a runner
on my laptop” asks once whether agents should run in the sandbox or with full
access. Sandbox is the default for a shared machine; `native-trusted` is
available for a dedicated machine. Sandbox restricts agent access, while full
access lets agents use the machine's files, credentials and network. Luna
carries the answer into the enrollment `isolation_tier` field and the
`detent hub runner register --isolation-tier` command. Enrollment tokens come
from the secure enrollment dialog, never from chat.

Fly Sprites are dedicated VMs and new Sprite runners use Full access
(`native-trusted`). An owner or admin can approve switching an individual Sprite
runner to Sandbox to request its isolation probe. Sandbox work waits until that
probe reports support. See [Sprite runners](sprite-runners.md).

For an existing runner, Luna reads its selected tier, backend support and
current problems. `tier_unavailable` means its agent backends do not support
the selected tier. An organization owner or admin can approve Luna's preview
to switch to a supported tier without enrolling again. Approval is always
required for runner access changes, even with ordinary chat confirmation off;
non-admin members cannot make this change. Projects requiring sandbox remain
ineligible for a `native-trusted` runner. Luna reads the runner again after the
change to verify the tier and identify any remaining problem.

## Organizations, roles and grants

| Role | Allowed without project grants |
| --- | --- |
| Owner | Membership and organization administration, ownership role changes, organization billing |
| Admin | Membership and organization administration, excluding owner changes |
| Member | Organization selection and explicitly granted project collaboration |
| Reporter | Filing issues into `Triage` and reading only the issues they filed |
| Viewer | Organization selection and explicitly granted project reads |

Project grants are explicit per user and project, with separate collaboration and runner-management flags. A viewer stays read-only even if a stored collaboration flag is true. Organization roles do not by themselves supply project content, code credentials, runner execution or check authority. Native runner-management operations require management grants on every project in the organization, because runner routing and listings can cover several projects; runner execution still requires a runner credential. A project creator receives access through an explicit self-access choice.

Each request validates the identity provider's active session and current membership, then local membership and project grants; provider unavailability fails closed. A provider-side change takes effect within one access-token lifetime. Native mutations recheck authorization inside their database transaction. Removing a member revokes local sessions and removes project and runner grants; re-inviting a removed member does not restore old grants. Removing or demoting the last owner is refused, and ownership transfer is a promotion followed by a demotion. An existing organization is joined only through current membership or a provider-validated exact-recipient invitation, never by matching a name or domain.

Hosted support staff can act inside a customer organization only through a time-limited, audited support session started for that organization, which never grants more than the customer role and grants being viewed and shows the actual and effective identity in the app. Operator metadata reports contain counts, sizes, health and plan fields, never issue bodies, titles, repository paths, prompts, source, diffs, logs or artifact contents. This is application authorization: the hosting operator remains technically able to read hosted data, and no zero-knowledge guarantee is made.

## Organization provisioning and recovery

`detent cloud serve` provisions organizations when its configuration has an `allocation` block (see [shared entry](examples/hub/README.md#self-service-provisioning)); without it the shared entry offers no creation and routes only registered tenants. The entry serves `/organizations/new`, `/organizations/ORG/provisioning` and `/organizations/ORG/delete`, and JSON at `/api/cloud/organizations/ORG/provisioning`.

A verified signed-in identity submits `POST /organizations` with a session-bound CSRF token and a stable creation idempotency key. The same user, key and payload return the same operation; a changed payload conflicts, and separate tabs retrying one intent cannot create duplicate owners. An organization display name is never identity or proof of ownership. The entry advances each intent through durable checkpoints with bounded retries (`retry_limit`, exponential backoff up to five minutes), and a restarted entry resumes pending intents:

| Checkpoint | Behavior |
| --- | --- |
| `requested` | Binds the creator's verified provider subject to the creation intent and enforces eligibility and per-identity limits |
| `allocating` | Reserves bounded tenant capacity, creates or recovers the WorkOS organization and owner membership, initializes the tenant database, and assigns the configured free plan without any Stripe customer or card |
| `ready` | Publishes the route only after database health, membership, allocation and plan references agree |
| `failed` | Keeps the intent, completed checkpoints and a bounded error; recoverable failures offer Resume without a new identity, and existing tenants stay healthy |
| `deleting` / `deleted` | Fences routing and writers and tracks erasure; tombstones prevent reuse |

Provider email changes do not transfer ownership. A failed signup does not erase customer data.

## Repository configuration and policy

Validation commands, review requirements and release policy are project
choices; see [how Detent delivers a change](project-delivery.md) for the stages
they control.

The customer checkout and its `detent.yaml` / `WORKFLOW.md` stay on the customer
host. Cloud does not write repository files. A supplied repository definition owns the native workflow;
Cloud Markdown authoring is the fallback when no repository definition exists.

### Workflow definition authority

The configured workflow path and `workflow_ref` select the definition anchor.
The split layout puts machine configuration in `detent.yaml` beside agent
instructions in `WORKFLOW.md`; legacy YAML frontmatter in `WORKFLOW.md` is also
supported. Markdown bullets are instructions, never structured configuration.
A prose-only `WORKFLOW.md` without `detent.yaml` supplies no structured workflow:
native runners load the Cloud-authored definition when one exists, otherwise
report that a definition must be supplied. Malformed, mixed-layout or invalid
repository definitions fail with parser feedback and never fall back to Cloud
or the hosted template. Inaccessible files, unavailable Git and invalid refs
remain instance failures, not evidence that the repository has no definition.

The resolved configuration supplies native states, allowed transitions, terminal
and dispatchable classification, runner scheduling, and Cloud Board, List and
lane-picker contents. An enabled plan contributes its configured stop (normally
`Plan Review`) as a nondispatchable lane. Empty lanes remain available.
Native defaults permit moves between configured states; explicit
`server.kanban.allowed_transitions` entries constrain their source states for
both authorized issue moves and execution progress, so constrained execution
states also need their completion and landing transitions.

The runner reports bounded state and transition metadata with the resolved
repository configuration. Reports from the bound repository's default-branch
history apply the shared workflow atomically with its source revision. States
occupied by issues, including archived issues, cannot be removed until those
issues move through ordinary authorized transitions.
Configuration changes never remap work items or revoke active leases.

Cloud settings show the repository source and revision and direct definition
edits to the repository. UI, API and MCP definition writes cannot override
repository authority. Authorized issue moves and other integration settings
remain available.

With no supplied repository definition, the Workflow settings editor accepts
Markdown whose frontmatter uses the same schema: `tracker.kind: hub_native`,
an ordered `tracker.lanes` list of `{name, role}` entries, and optional
`server.kanban.allowed_transitions`. The retired `tracker.active_states`,
`tracker.observed_states` and `tracker.terminal_states` keys are rejected. The
body contains agent instructions. Cloud persists that Markdown and derives its lanes with the
existing parser. Runners load and report the resolved definition. Adding a
repository definition transfers workflow authority without losing work items.
Repository-controlled definitions cannot be replaced by Cloud Markdown or
independent state arrays.

A Cloud runner uses `client.hub_url: https://cloud.detent.build/organizations/ORG`
and `client.organization_id` set to the immutable organization ID. Its projects
come from enrollment `project_ids` or [runner routing](hub-api.md#runner-routing-authority);
the project's page shows its native ID. `detent doctor` checks that configuration
locally, and the agent provider sign-in lives on that host. Cloud saves only
user-reported checkbox acknowledgments of these local checks; they do not
override runtime validation.

Runners report the configuration resolved from `detent.yaml` and `WORKFLOW.md`.

<!-- BEGIN POLICY APPROVAL RULE -->
Repository configuration reports do not require approval before dispatch.
Trusted workflow definitions from the bound repository's default-branch history
apply automatically. Explicit approval is required for administrative
configuration adoption, such as applying an external local definition or changing
local-binding permission. Configuration identity changes do not revoke active
leases or invalidate recovered source.

On a self-hosted Hub without the web app, read `get_project_policy` and
`get_onboarding`, then use `approve_project_policy` with the current
`expected_policy_id` and the chosen `observed_policy_id`. The REST equivalent is
GET and PUT on `/api/v2/organizations/{organization}/projects/{project}/policy`,
with GET on the project's `/onboarding` for observed candidates. Approval needs
administrator authority and preserves revision checks. Cloud owners and admins
can continue approving in **Settings > Integrations**.
<!-- END POLICY APPROVAL RULE -->

See [policy approval without the web app](api-mcp-setup.md#policy-approval-without-the-web-app)
for the complete API/MCP procedure.

Validation, review, auto-promotion and merge settings live in the repository;
the runner reports the resulting configuration when it reloads. Configuration
identities remain attached to attempts and published source for provenance.
A configuration ID change does not revoke an active lease, discard a checkpoint,
or send completed work back to Rework.

Runner enrollment, organization and project access, execution isolation, lease
fencing and the project's validation and review requirements still apply.
Full-machine execution is the operator's chosen trust boundary; a second
repository-policy approval is not required to use it. The legacy policy-revocation
operation is retired and returns HTTP 410 without deleting configuration.
Pausing or draining a runner, or removing its project access, stops execution.

A native project (`tracker.kind: hub_native`) makes no GitHub REST or GraphQL
calls by default. The runner does not give the agent the instance GitHub
credential, open or look up pull requests, or ask the agent for Workpad comments;
it opens the native Change Request from the attempt's commits after a successful
run, and the local gate is the CI. Git transport (clone, fetch) still works. The
agent prompt ends with a native completion contract that overrides GitHub steps
in `WORKFLOW.md`. `detent doctor` warns when the
repository `WORKFLOW.md` still describes GitHub steps. Setting `worker.github_token` explicitly in the
project is the only way to give its workers GitHub access.

## Customer host enrollment

In Settings > Providers & runners, choose Add runner > Manual to enroll a
runner, give it a name and pick its projects. The dialog preselects macOS or
Linux from your browser; switch it if the target machine uses the other OS.
Install Detent before creating the one-time token, which expires after 15
minutes. The existing installer selects the latest release for macOS or Linux
and detects amd64 or arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
```

On macOS, Homebrew is an alternative:

```sh
brew install digitaldrywood/tap/detent
```

The dialog shows the Hub's minimum runner version. Check `detent version` on
an existing installation and, if older, run `detent update --yes --from-release`
or `brew upgrade digitaldrywood/tap/detent` for Homebrew before registering.
The registration output also tells older binaries to update.

For a fresh machine, copy the dialog's combined install and register command.
It adds the install directories to PATH so the new binary is available in the
same shell:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh && \
  export PATH="$HOME/.local/bin:/usr/local/bin:$PATH" && \
  detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID \
  --token TOKEN --name "Build host" --service
```

If Detent is already installed, copy the register command alone:

```sh
detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID \
  --token TOKEN --name "Build host" --service
```

AI assisted setup with Luna gives the same installation guidance before
registration on your own machine. Keep the token out of chat; the dialog masks
it by default and copies it only into the command you run on the target host.

The register command generates the host's identity locally, redeems the one-time token,
writes `~/.config/detent-runner/global.yaml` and `identity.json`, and installs
the `detent.runner` background service. Clone each project's repository into
the directory it prints (`~/detent-runner/PROJECT` by default)  first, or run the
`detent start --config ... --yes` command it prints after cloning. Nobody copies
a runner or machine ID by hand. See [register a runner with one command](hub-api.md#register-a-runner-with-one-command)
for what each step writes.

Enrolling needs runner management on every organization project. An owner or
administrator who creates a project receives it automatically; grants for other
members are set in Organization.

Retry `register` with the same configuration directory: it reuses the identity
it generated, never creates a second runner, and never rewrites an existing
`global.yaml`. Enrolled runners reject unknown configuration keys at startup and
name their full paths, including nested machine settings. `projects`, `weight`,
`priority`, `scheduling`, `fair_share`, `global.agents.model_selection`,
`client.native_projects` and other local project policy defaults are rejected,
so a runner file containing them does not start. Cloud owns project membership
and model policy. An enrolled identity file belongs to one machine. The
older `detent hub runner init` and `enroll` commands, and the
`--host-identity-file` protocol for several runners on one host, remain for
scripted setups.

The page lists only runners authorized for this project. Names, host IDs, tags,
health and exclusion reasons come from the existing routing evaluator. Tag edits
preserve the runner's full access scope and use its expected revision. Other
projects' grants, leases and provider account records are omitted from readiness.

### Runner in a Fly Sprite

Inside a fresh Sprite, run
[sprite-runner-bootstrap.sh](../scripts/sprite-runner-bootstrap.sh) and paste
the Hub's enrollment command when it prompts:

```sh
bash scripts/sprite-runner-bootstrap.sh
```

The prompt does not echo. Without a terminal, send the command on standard
input, for example from a private file you delete afterwards. The script splits
the command without a shell and rejects `$`, backquotes and shell operators. It
passes the token to `detent hub runner register` through
`DETENT_RUNNER_ENROLLMENT_TOKEN`, so the token never appears in an argument
list, the service launcher or the script's output.

The script installs the Go toolchain named in `go.mod` (the adjacent checkout's,
`--go-mod PATH`, or the pinned tag's), `gh`, the npm builds of Codex and Claude
Code, and the checksum-verified Detent `v0.117.41` release. `--version vX.Y.Z`
selects another release; `--from-source /path/to/detent` builds a checkout
instead and is never the default. Registration uses the documented defaults,
`~/.config/detent-runner/global.yaml` and workspace root `~/detent-runner`,
unless the command or `--config` and `--workspace-root` name others.

The host `--service` flag is replaced by a Sprite Service named
`detent-runner` that runs `detent --headless` in the foreground. The Sprite
runtime restarts it after a cold wake or crash and resumes it after a warm
wake, so polling continues whenever the Sprite runs. Whether the runner's
outbound polling alone keeps a Sprite from pausing is not yet verified; a
paused runner picks up work only after its next wake.

Re-running is safe. With empty input the script reuses the registered runner
and restarts the service. With a pasted command, `register` keeps the existing
identity and `global.yaml`, so no second runner is created. One runner per
Sprite is supported, and its identity belongs to that Sprite.

The script creates an installation checkpoint with `sprite-env checkpoints create`
and prints the ID. This does not verify authentication, registration health,
project setup or a real task. Failure output names the last completed step and
resume command. Follow the [complete Sprite guide](sprite-runners.md) to verify
those steps and save the final ready checkpoint. Restore
the same Sprite with `sprite-env checkpoints restore CHECKPOINT_ID`. A failed
checkpoint fails the script but leaves the enrollment and service in place.

Remaining manual steps, which the script never performs:

- Sign in to the providers the project uses: `claude auth login`, `codex login`
  (`codex login --device-auth` without a browser), or a Sprites provider
  connector.
- For private clones and pushes, run
  `gh auth login --hostname github.com --git-protocol https --web` and `gh auth setup-git`,
  or set up the project's own git credentials, and set the git author.
- The runner clones allowed projects into `workspace_root/PROJECT_ID` itself and
  runs repository-declared `hooks.runner_setup`; no separate manual clone is
  needed. See [Sprite setup](sprite-runners.md#7-prepare-git-access-and-the-project-checkout).
- The project's reported repository policy appears in the Hub; policy provenance
  is not a recurring claims or publication gate.
- `sprite-env services get detent-runner` and the Hub's runner health report the
  service state.
- A new checkpoint after sign-in captures the sign-ins and checkouts.

### Runner restart and claim recovery

The default claim lease is ten minutes, renewed every thirty seconds. This
window covers a normal update's drain, binary swap and reconnect. Draining or
interrupted runners retain in-flight claims until their leases expire, including
when a policy or heartbeat error interrupts execution. Other runners cannot
claim that work during the live lease.

A returning runner renews its retained leases with their existing fencing tokens
and resumes the recorded attempts, local worktrees and provider sessions. If it
does not return before expiry, any eligible runner can claim the work with a new
fencing token and recover from the pushed branch and Change evidence. Revocation
or removal releases the runner's claims immediately. An explicitly configured
`client.lease_ttl_seconds` continues to determine the lease window.

## Artifact history

Artifact history has three modes, and no setup default silently uploads artifacts. Local history keeps artifacts on the runner without a storage account or fee; losing that machine loses its artifacts. Customer-managed history uses an S3-compatible bucket and an artifact gateway the customer runs and pays for; Detent records only the binding (gateway origin, service ID and publisher token ID) and never stores the customer's artifacts, the publisher token secret or storage credentials. Hosted artifact storage is an explicit opt-in on plans with the `hosted_artifacts` feature (see [hosted allowances](hosted-allowances.md#hosted-artifact-storage-and-relay)). A registered binding is configuration evidence, not a storage-health or offline-availability guarantee. See [self-hosted artifacts](hub-self-hosting.md#optional-durable-artifacts) for the customer-managed gateway.

## GitHub

Native issue creation works with GitHub disabled. To connect GitHub:

1. Associate the repository as `owner/name` in Project setup. An enrolled runner
   clones it using its own Git credentials; setup shows progress until the
   runner reports the matching checkout.
2. [Install the Detent Cloud GitHub App](https://github.com/apps/detent-cloud/installations/new)
   on the repository or organization, with all repositories or a selection that
   includes this project's repository. The App requests Issues and Pull requests
   read/write, and Contents, Checks, Commit statuses and Metadata read permissions.
3. Project setup then shows whether Detent Cloud is installed on `owner/name`.
   New GitHub issues enter `Triage` automatically once the App is installed.

Manual imports, summary projection and repository/PR integration are separate choices; saving setup progress does not activate an integration. See [GitHub-compatible projects and native cutover](hub-local-migration.md#github-compatible-projects-and-native-cutover) for intake, import and profile details.

## Project setup API

A self-hosted Hub uses the same scoped API with its own authentication, without a Detent Cloud account, subscription or Stripe connection:

- `GET /api/v2/organizations/ORG/projects/PROJECT/onboarding` reads persisted
  progress, current approved policy, eligible runners, bindings and latest run.
- `PUT` to the same path accepts `idempotency_key` and `progress`, containing
  string `revision`, `repository` (`existing` / `generate`), booleans `doctor`
  and `provider`, and `artifacts` (`local` / `customer`).
- Writes compare revisions and reuse the native idempotency journal. Browser
  retries keep a command key and payload fingerprint in session storage, bound to
  organization, project and user; no issue body or provider credential is stored
  there.
- The `/onboarding/policy`, `/onboarding/artifact-services/SERVICE`,
  `/onboarding/integration` and `/onboarding/repository` adapters require scoped
  administration and call the existing validated implementations.

A stale revision returns 409, invalid input returns 422, and access failures name the missing account or grant. The first native issue uses the ordinary idempotent work-item API; its state determines whether it queues, and policy and runner checks remain authoritative for execution.

## Independent Change checks

A project's approved Change review policy can require `independent` checks submitted by a dedicated CI credential. Hosted admission lets an `operator` bearer submit only `POST /api/v2/organizations/ORG/projects/PROJECT/work-items/ITEM/changes/CHANGE/versions/VERSION/checks`, and only when the credential is active, has an explicit current grant on that project, and is named as `principal_id` by an `independent` required check in the current approved review policy. The exception grants no reads, publication, review decisions, policy changes, imports or token administration. Administrator bearers, execution workers, browser sessions and unapproved operators cannot substitute, and enrolled runners can satisfy only `customer` checks.

An owner or admin approves the review policy with `PUT /api/v2/organizations/ORG/projects/PROJECT/change-review-policy`, sending `idempotency_key`, the current `expected_review_policy_id` when replacing a policy, and `policy` with the approved repository `policy_id`, `require_review` and `required_checks`. Each check pins `name`, the credential ID as `principal_id`, `workflow_id`, `workflow_sha256`, `source: "independent"` and `max_age_seconds` (60–604800). Results are submitted with a unique idempotency key to the exact version's `/checks` route and are validated against the version's expected principal, head, run, policy, configuration, check-run ID, workflow digest and completion time; changed terminal results conflict.

Replacing the review policy with a different principal immediately stops the old principal's submissions, including for old versions, which keep their immutable policy and show stale-policy readiness. Removing the project grant also denies submissions. Evidence loses readiness at its freshness bound. Human-review projects also need current-version human approval. Native `reviewed` status does not authorize an external merge or bypass GitHub branch protection.

Hosted token creation, rotation (`POST /api/v1/tokens/ID/rotate`, which keeps the principal ID and grants), revocation (`DELETE /api/v1/tokens/ID`) and project grants (`POST /api/v2/tokens/ID/grants`) are served only by the private loopback `detent hub serve --credential-maintenance` listener for the bound hosted tenant, authorized by the instance administrator credential; public hosted serving denies instance token administration.
