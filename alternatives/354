# Migrate a local board to a local Hub

This runbook moves an existing `projects:` board, its GitHub or local tracker,
and its Templ dashboard to a self-hosted Hub with enrolled runners. Since
v0.132.0, a non-`hub_native` board without `client.hub_url` refuses to start.
Adding a URL alone does not migrate its issues, lanes or credentials.

Use a reviewed release containing the features your migration needs. The
[local-mode Hub work](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1373)
tracks the remaining prerequisites:

| Existing setup or requirement | Prerequisite before completing cutover |
| --- | --- |
| `local_sqlite` or `github_local` lanes/history | [#1377: local store import](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1377) |
| GitHub ProjectV2 status lanes | [#1378: ProjectV2 lane intake](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1378) |
| Browser dashboard without WorkOS | [#1374: self-hosted web app](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1374) |
| Organization model settings on an unhosted Hub | [#1385: organization model selection](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1385) |
| Cloud and local-Hub runner services on one host | [#1380: separate runner services](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1380) |

Until the corresponding implementation ships, stop at that step. Do not replace
local lane state with guessed labels, edit either SQLite schema, or configure
WorkOS merely to get a local dashboard. The API/MCP Hub works headlessly today;
its browser dashboard is not the old Templ board. For Linux service installation
and TLS see [Hub operations](hub-self-hosting.md); the single-machine macOS and
launchd guide is tracked by
[#1379](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1379).

## 1. Pin and preserve the old board

Before allowing an updater to replace it, retain the last working board binary,
its version/checksum and installation files. Record the actual service executable
and configuration path. An already refusing binary cannot serve as your rollback
binary; retrieve a verified board-capable release compatible with your saved
configuration and database. Never downgrade a live database in place.

Inventory every configured project and its tracker before changing anything.
Include paths outside the global configuration directory:

| Preserve | Contents |
| --- | --- |
| Whole global config directory | `global.yaml`, private overrides, `detent.db`, and referenced state paths |
| Every `tracker.local_sqlite` file | Local issue bodies, lane state and history, including `github_local` stores; resolve paths using the old configuration |
| Every repository | `detent.yaml`, `WORKFLOW.md`, `detent.local.yaml` if present, current revision and unfinished branches/worktrees |
| Service configuration | launchd plist or systemd unit/drop-ins, environment file, executable path and enablement state |
| Credentials | Separate private backup of provider/Git sign-ins and secret configuration; never put secrets in issue exports |

Pause scheduling and let active attempts and landing operations finish before
stopping the old board. If it still runs, use its existing pause control or its
pinned binary's pause command. If it already refuses, keep its service stopped;
do not repeatedly restart it or bypass the refusal.

With all board/database owners stopped, copy the complete configuration and
tracker files to restricted, durable backup storage. Retain any SQLite WAL/SHM
sidecars as a coherent set from that stopped owner; do not copy individual live
database files. Include the repository and service files above, even when located
elsewhere. Verify that the copies are readable and keep an untouched backup.
Resume the pinned old board only if it supports the saved configuration; keep
the pilot paused. Other projects can remain there during the pilot.

## 2. Start a loopback Hub and connect MCP

Use the new released binary for the Hub, separately from the pinned board
executable. Choose unused loopback ports and new private durable paths; do not
point the Hub at the old `detent.db` or any tracker database. The following
single-machine example uses the same OS user for Hub and runner:

```sh
umask 077
export HUB_URL=http://127.0.0.1:7777
export HUB_DIR="$HOME/.local/share/detent-hub"
mkdir -p "$HUB_DIR/backups"
export DETENT_HUB_ADMIN_TOKEN="$(openssl rand -hex 32)"
detent hub serve --database "$HUB_DIR/hub.db" --listen 127.0.0.1:7777 --github-disabled
```

Store the generated token in your secret manager before ending this shell; it
is the bootstrap administrator credential. Load that same value into subsequent
shells without printing it or putting it in arguments/history. Startup inserts
it once; setting a different environment value later does not rotate it. Follow
[authentication and rotation](hub-self-hosting.md#authentication-projects-and-runners)
for later changes. Keep the Hub in the foreground initially; stop it with Ctrl-C
and wait for exit before maintenance.

In a second shell, load those variables and check authenticated health. Passing
the header through standard input keeps the token out of process arguments:

```sh
curl --fail --silent --show-error --config - <<EOF
url = "$HUB_URL/health"
header = "Authorization: Bearer $DETENT_HUB_ADMIN_TOKEN"
EOF
```

Repeat the authenticated request with URL `$HUB_URL/api/v2/organizations` and
select the returned local organization ID. Configure a local Streamable HTTP
MCP client with URL
`http://127.0.0.1:7777/api/v2/organizations/ORG_ID/mcp` (substitute that ID) and an
`Authorization: Bearer` header supplied from its
private environment/secret facility. Remote hosted clients cannot reach your
machine's loopback address. Complete MCP initialization, list available tools,
then use the advertised discovery tool to select the organization and project
toolsets. Use [API & MCP setup](api-mcp-setup.md) and
[capability availability](mcp-capabilities.md): tools backed only by hosted
application services may be unavailable on an unhosted Hub. Use the documented
native API for those operations, not invented MCP tool names or Cloud URLs.
Create a narrower operator/project credential for routine use after bootstrap.

Take the first backup before creating projects or enrolling runners. Stop this
Hub and wait for exit, then run:

```sh
detent hub backup --database "$HUB_DIR/hub.db" --output "$HUB_DIR/backups/initial.db"
detent hub verify --database "$HUB_DIR/backups/initial.db"
```

The destination must not exist. Restart with the same serve command and token.
Keep completed snapshots in encrypted backup storage with the binary version
and separate secret backups. These commands require a stopped Hub; online
snapshots are tracked by
[#1388](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1388).
Follow [backup and restore](hub-self-hosting.md#backup-and-export) for later
snapshots, artifacts and restore isolation.

## 3. Quiesce one pilot and create its native project

Choose one small repository first. On a running, pinned old board, substitute
its real config path and project ID:

```sh
detent --config /private/board/global.yaml pause example-project --reason "Hub migration pilot"
```

This command belongs to the old board installation. Confirm the project is
paused, has no active workers or landing, and has no ambiguous pending external
effects. Refresh its stopped-owner backup after the drain. If stopping that
board pauses other projects, arrange their maintenance window too.

As Hub administrator, list `GET /api/v2/organizations` and select the returned
local organization ID. Create the pilot with
`POST /api/v2/organizations/{organization_id}/projects` using the exact request
contract in [native project setup](hub-api.md#native-organization-and-project-setup).
Save the returned opaque organization/project IDs; the old board's project name
is not the new ID. Use a stable `idempotency_key` for retries.

Supply a workflow matching the repository's intended review and landing path,
including a nonterminal, nondispatchable Backlog for imports. Match actual lane
names, transitions, dispatchability and terminal flags, not just their labels.
Keep all imported work there until checks finish. Do not use the API example's
operator-only Review lane as an automated review lane: the runner's configured
review source must be reachable and not operator-only. Preserve the project's
existing independent review/security requirements.

## 4. Prepare the repository and enroll a runner

In the pilot's `detent.yaml`, replace the old tracker configuration with:

```yaml
tracker:
  kind: hub_native
```

Retain repository execution policy, gate, review requirements and landing target.
Remove obsolete tracker credentials and local store options from the active
configuration, preserving their backups. Update `WORKFLOW.md` to the native
worker handoff rather than GitHub issue/PR/comment instructions. Review private
`detent.local.yaml` overrides so they cannot restore the old tracker. Publish
these reviewed repository files on its intended default branch through the
repository's normal change process before the enrolled runner loads them.

Native Changes land through git by default. If this project intentionally uses
GitHub pull requests, set this optional top-level policy in `detent.yaml`:

```yaml
deliverable:
  github_pull_request: true
```

This selects a deliverable; it does not make GitHub the issue authority. Preserve
the project's GitHub credentials, target branch and required protections. See
[GitHub profiles](github-profiles.md) for repository binding and intake, and
[#1384](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1384)
for the self-hosted App/webhook setup work. The Hub may keep `--github-disabled`
when using runner-backed intake; this does not disable runner Git transport.

Before enrolling, read the project's integration revision with
`GET /api/v2/organizations/{organization_id}/projects/{project_id}/integration`.
Bind its repository with `POST .../projects/{project_id}/onboarding/repository`,
supplying `idempotency_key`, the returned `expected_revision` as a string,
`repository: "example-org/example-project"` and `source: "runner_checkout"`.
Substitute the pilot's repository. This records the native checkout binding
without a Hub-side GitHub fetch; runner readback will establish checkout health.

Create a short-lived, single-use enrollment through
`POST /api/v2/organizations/{organization_id}/runner-enrollments`, granting only
the pilot project and `read`, `claim`, `heartbeat`, `events`, `collaborate`.
For token-first registration omit runner/machine IDs; use the exact
[enrollment contract](hub-api.md#scoped-runner-onboarding). Deliver the returned
token privately as `DETENT_RUNNER_ENROLLMENT_TOKEN` to the runner shell.

Use a fresh runner config directory and workspace root, outside the old board's
paths and any existing Cloud runner installation:

```sh
detent hub runner register --url "$HUB_URL" --organization org_example --name "Local pilot" --capacity 1 --config "$HOME/.config/detent-local-runner/global.yaml" --workspace-root "$HOME/detent-local-runner"
unset DETENT_RUNNER_ENROLLMENT_TOKEN
detent --config "$HOME/.config/detent-local-runner/global.yaml" --headless
```

Replace `org_example` with the returned organization ID. Registration writes
the identity and machine-only config; keep both private. Do not copy the old
`projects:` list, `client.native_projects`, model selection or scheduling defaults
into this config: enrolled runners reject them. Hub grants supply the projects.
Keep provider sign-ins and Git authentication on the runner host.

Confirm the binding, checkout and source revision on readback; the integration
read's `checkout_verified` reflects a recent matching authorized runner report.
Follow [runner onboarding](hub-api.md#register-a-runner-with-one-command) and
the headless enrollment documentation work in
[#1386](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1386).
Do not add a local project map to make a missing checkout appear. Start with
the foreground runner; service coexistence with an existing Cloud runner awaits
#1380. Installing a service must not replace the old board's rollback unit.

## 5. Import open work without dispatching it

Compare against the frozen source inventory, including open issue IDs, bodies,
comments, dependencies, lane names and blocked/held work. Keep original source
identities/provenance; do not recreate issues by hand under new identities.

- **GitHub issues with label lanes:** use
  [selective onboarding intake](hub-api.md#selective-onboarding-issue-intake).
  Discover/preview first, choose only open issues, map labels explicitly and
  apply into Backlog. Do not authorize dispatchable destinations during import;
  label mappings must not accidentally route work into Todo. Follow all pages
  and inspect failed, skipped and partial results before proceeding. Import
  dependencies too; preserve holds and unresolved references.
- **`local_sqlite` or `github_local`:** stop until the importer from #1377 is
  available in your selected release. GitHub intake alone cannot recover local
  lanes, local-only issues or local history. Keep the store backup and use that
  importer's documented preview/apply contract when shipped.
- **GitHub ProjectV2:** stop if preserving its status-field lanes is required
  until #1378 ships. Current label intake is not a ProjectV2 status migration.

Verify retained identities, discussion, dependencies and lane mapping against
the source inventory, not only total counts. Inspect explicit gaps; an import
receipt does not prove inaccessible source history was recovered. Keep the old
pilot paused, and keep new intake from dispatching work until reconciliation.

## 6. Select models, approve policy and land one issue

Set model/backend/reasoning choices in the Hub's organization settings and, if
needed, the pilot project's override. Read back the effective stage selections
before dispatch. Organization model selection for an unhosted Hub is tracked by
#1385; if the selected release does not support your required settings, stop
here. Do not work around it with model keys in global or repository YAML.

Read the runner's observed repository policy and onboarding status through the
Hub. Check source revision, gate command, landing target, review/security policy
and runner grants. Approve the exact trusted descriptor through the supported
project policy API/MCP owner described in
[approved repository policy](hub-api.md#approved-repository-policy).
The consolidation of self-hosted approval instructions is tracked by
[#1383](https://cloud.detent.build/drywood-creek-consulting/detent/issues/1383).
Do not introduce a rejected `client.native_projects` mapping to run an old
approval example. Approval is not permission to bypass review or the gate.

Create one small native issue with checkable acceptance criteria, move only that
issue to the configured dispatchable Todo lane through the Hub, and watch its
attempt, Change version, review, security audit, gate and landing records. Require
a successful landing on the intended target branch and the expected terminal
state before enabling imported work. Do not replay an old in-flight attempt on
the new Hub or operate the same issue from both boards.

## 7. Verify and repeat, or roll back

Before expanding the pilot, verify:

- Authenticated health and MCP reads survive a normal Hub restart with the same
  database and token; the pilot and imported identities remain unchanged.
- Exactly the intended runner has project access, a valid renewed identity,
  healthy provider/Git setup and the expected checkout/policy/model selections.
- Imported open work, discussion, dependencies, holds and lanes reconcile with
  the inventory; no partial imports or missing required lane mappings remain.
- The pilot Change passed the configured checks and landed once, and the old
  board has not resumed its pilot or projected conflicting state to GitHub.
- A fresh stopped-owner Hub backup verifies. Rehearse an isolated restore using
  [the restore procedure](hub-self-hosting.md#restoreimport-and-move-between-hubs);
  restore revokes copied credentials, so reenrollment is required.

For rollback, stop new dispatch on the Hub through its existing project controls,
let active work settle, then stop the pilot runner. Reconcile pending external
effects and any commits or GitHub PRs already created; a database restore cannot
undo them. Keep a verified Hub snapshot for diagnosis. Never run both owners for
the pilot while restoring authority.

Restore the old board's saved configuration, repository definitions and private
overrides, service file and matching pinned binary. Restore databases only from
their coherent stopped-owner backup into an isolated destination first; do not
overwrite a running database. Preserve newly landed commits and explicitly
reconcile their corresponding old issues before resuming. Keep the Hub pilot
nondispatchable or its runner stopped, verify the pinned board reads the expected
projects and lanes, then resume only the intended project. If your saved binary
is already subject to the startup refusal, it is not a usable rollback target.

After a successful pilot, repeat the inventory/import/reconciliation and single
issue verification for each remaining project. Retire the old board only when
all projects have migrated: disable its launchd/systemd startup and auto-updater,
retain the pinned recovery bundle for your retention window, and remove obsolete
credentials only after checking they are not shared with a runner. Keep the Hub
and runner services separate and establish regular verified backups. Do not
delete local tracker stores merely because the new board displays issue titles.
