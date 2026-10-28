# Migrate a local board to a Hub

The standalone local board is retired. A `global.yaml` that lists projects whose tracker is not `hub_native` and has no `client.hub_url` refuses to start with "Standalone local board projects are no longer supported without client.hub_url", and `detent doctor` reports the same condition as a warning. Adding a URL alone does not migrate issues, lanes or credentials. This page describes the supported migration of an existing `projects:` board (with a `github`, `local_sqlite` or `github_local` tracker) onto a Hub with enrolled runners, either Detent Cloud or a self-hosted Hub, and the cutover of a `github_compatible` Hub project to the native profile.

The Hub serves its web app without WorkOS, runs headless through the API and MCP, and can share one machine with its runner; see [Hub self-hosting](hub-self-hosting.md) for installation, TLS, the single-machine macOS and Linux service layouts, and backups. Use a reviewed release containing the migration features below:

| Existing setup or requirement | Procedure |
| --- | --- |
| `local_sqlite` or `github_local` lanes/history | [Local store import](#5-import-open-work-without-dispatching-it) |
| GitHub ProjectV2 status lanes | [Selective onboarding intake](hub-api.md#selective-onboarding-issue-intake) |
| Browser dashboard without WorkOS | [Local web app](hub-self-hosting.md#open-the-web-app-without-workos) |
| Organization model settings on an unhosted Hub | [Model selection](hub-self-hosting.md#model-selection) |
| Cloud and local-Hub runner services on one host | [Separate Hub and runner services](hub-self-hosting.md#single-machine-hub-and-runner) |

For one Mac, follow the [macOS/launchd guide](hub-self-hosting.md#macos-with-launchd). Preserve local lane state through import rather than guessed labels or direct SQLite schema edits.

## 1. Preserve the old board

A binary that already refuses to start cannot serve as a rollback target, so the last board-capable binary, its version/checksum and installation files are kept separately. A live database is never downgraded in place.

The migration inventory covers every configured project and its tracker, including paths outside the global configuration directory:

| Preserve | Contents |
| --- | --- |
| Whole global config directory | `global.yaml`, private overrides, `detent.db`, and referenced state paths |
| Every `tracker.local_sqlite` file | Local issue bodies, lane state and history, including `github_local` stores; resolve paths using the old configuration |
| Every repository | `detent.yaml`, `WORKFLOW.md`, `detent.local.yaml` if present, current revision and unfinished branches/worktrees |
| Service configuration | launchd plist or systemd unit/drop-ins, environment file, executable path and enablement state |
| Credentials | Separate private backup of provider/Git sign-ins and secret configuration; secrets never belong in issue exports |

Copies of SQLite stores are coherent only when taken from a stopped owner, with any WAL/SHM sidecars as one set. Active attempts and landing operations finish before the old board stops; an old board that still runs can pause projects with `detent --config <old-config> pause <project> --reason "Hub migration"`.

## 2. Start a Hub and connect MCP

The Hub uses its own private durable paths; it never opens the old `detent.db` or a tracker database. A single-machine loopback example:

```sh
umask 077
export HUB_URL=http://127.0.0.1:7777
export HUB_DIR="$HOME/.local/share/detent-hub"
mkdir -p "$HUB_DIR/backups"
export DETENT_HUB_ADMIN_TOKEN="$(openssl rand -hex 32)"
detent hub serve --database "$HUB_DIR/hub.db" --listen 127.0.0.1:7777 --github-disabled
```

`DETENT_HUB_ADMIN_TOKEN` is the bootstrap administrator credential. Keep the generated value in a secret manager and load the same value for subsequent starts. Startup inserts it once; a different environment value later does not rotate it (see [authentication, projects and runners](hub-self-hosting.md#authentication-projects-and-runners)). The web app is at `$HUB_URL/work`, using `operator` as the HTTP authentication username and the administrator token as the password.

An authenticated `GET $HUB_URL/health` confirms the Hub is up, and `GET $HUB_URL/api/v2/organizations` returns the local organization ID. A Streamable HTTP MCP client connects to `$HUB_URL/api/v2/organizations/ORG_ID/mcp` with an `Authorization: Bearer` header; see [API & MCP setup](api-mcp-setup.md) and [capability availability](mcp-capabilities.md), since tools backed only by hosted services are unavailable on an unhosted Hub.

A running self-hosted Hub writes online snapshots through `POST /api/v2/admin/backup`; the offline `detent hub backup` and `detent hub verify` commands require a stopped Hub. See [backup and export](hub-self-hosting.md#backup-and-export).

Take the first backup before creating projects or enrolling runners. Stop this Hub and wait for exit, then run:

```sh
detent hub backup --database "$HUB_DIR/hub.db" --output "$HUB_DIR/backups/initial.db"
detent hub verify --database "$HUB_DIR/backups/initial.db"
```

The destination must not exist. Restart with the same serve command and token. Keep completed snapshots in encrypted backup storage with the binary version and separate secret backups. Subsequent backups can use the online snapshot API while the Hub runs.

## 3. Create the native project

Choose one small repository as a pilot. Pause it on the pinned old board, let workers and landing finish, reconcile ambiguous external effects, and refresh its stopped-owner backup before transferring authority. Arrange a maintenance window if stopping the old board also pauses other projects.

`POST /api/v2/organizations/{organization_id}/projects` creates the project using the request contract in [native project setup](hub-api.md#native-organization-and-project-setup); the returned opaque organization and project IDs replace the old board's project name. A stable `idempotency_key` makes retries safe. The project's workflow defines lane names, transitions, dispatchability and terminal flags; a nonterminal, nondispatchable `Backlog` keeps imported work out of dispatch until it is reconciled. The runner's review source lane must be reachable and not operator-only.

## 4. Prepare the repository and enroll a runner

The repository's `detent.yaml` selects the native tracker:

```yaml
tracker:
  kind: hub_native
```

Repository execution policy, gate, review requirements and landing target carry over. Old tracker credentials and local store options no longer apply, and `detent doctor` warns when `WORKFLOW.md` still describes GitHub-only steps. Native Changes land through git by default; `deliverable.github_pull_request: true` selects GitHub pull request delivery without making GitHub the issue authority. The runner loads the repository files from the bound repository's default branch.

`GET .../projects/{project_id}/integration` returns the integration revision. `POST .../projects/{project_id}/onboarding/repository` with `idempotency_key`, the returned `expected_revision` as a string, `repository: "example-org/example-project"` and `source: "runner_checkout"` records the checkout binding without a Hub-side GitHub fetch. The integration read's `checkout_verified` reflects a recent matching runner report.

`POST /api/v2/organizations/{organization_id}/runner-enrollments` creates a short-lived, single-use enrollment granting the project and `read`, `claim`, `heartbeat`, `events`, `collaborate`; see the [enrollment contract](hub-api.md#scoped-runner-onboarding). For token-first registration omit runner/machine IDs. Deliver the token privately as `DETENT_RUNNER_ENROLLMENT_TOKEN`. Use a fresh runner config directory and workspace root outside the old board and any Cloud runner installation:

```sh
detent hub runner register --url "$HUB_URL" --organization org_example --name "Local runner" --capacity 1 --config "$HOME/.config/detent-local-runner/global.yaml" --workspace-root "$HOME/detent-local-runner"
unset DETENT_RUNNER_ENROLLMENT_TOKEN
detent --config "$HOME/.config/detent-local-runner/global.yaml" --headless
```

Replace `org_example` with the returned organization ID. Registration writes the identity and a machine-only config. Enrolled runners reject the old `projects:` list, `client.native_projects`, model selection and scheduling keys; Hub grants supply the projects. Provider sign-ins and Git authentication stay on the runner host. A separate `service_name` lets a self-hosted Hub runner and a Cloud runner run as separate services on one host. See [register a runner with one command](hub-api.md#register-a-runner-with-one-command).

Confirm the binding, checkout and source revision on readback; follow [headless enrollment](hub-self-hosting.md#enroll-a-runner-without-the-web-app). Start with the foreground runner, then use `detent hub runner register --service` with separate config and workspace roots for each Hub. Keep the old board's rollback service intact; follow the [service coexistence guide](hub-self-hosting.md#single-machine-hub-and-runner).

## 5. Import open work without dispatching it

Imports keep original source identities and provenance.

- **`local_sqlite` or `github_local` stores:** `detent hub issue import-local <sqlite-copy> --project <project_id> --kind local_sqlite|github_local --store-id <stable-name>` imports a copy of the retired store. `--dry-run` reports without writing, `--lane-map Source=Target` maps lanes, and `--terminal-state` names source terminal lanes, which are imported only when needed as prerequisites. Each item is its own transaction and replays safely with the same `--store-id`. Preview with `--dry-run` before applying with the same store identity and lane mappings. During reconciliation, map dispatchable source lanes to Backlog, for example `--lane-map Todo=Backlog`. For `github_local`, complete GitHub intake into Triage first so the importer matches original issue numbers without duplicates. The scoped endpoint is `POST /api/v2/organizations/{organization}/projects/{project}/local-board-import`.
- **GitHub issues with label lanes:** [selective onboarding intake](hub-api.md#selective-onboarding-issue-intake) discovers, previews and applies selected issues with explicit label mappings. Apply into Backlog and keep `allow_dispatch` disabled during import. Follow all pages and inspect failed, skipped and partial results; import dependencies and preserve holds and unresolved references.
- **GitHub ProjectV2 status lanes:** the same intake accepts a `project_v2` source with `status_lanes`; ProjectV2 Status then takes precedence over label routing. Map Status values to nondispatchable lanes; inspect `project_status` in previews and `unmapped_project_status` in results. Missing or unmapped Status uses the intake destination.

Verify identities, discussion, dependencies, holds and lanes against the frozen inventory. An import receipt does not prove inaccessible source history was recovered; results report failed, skipped and partial items and explicit gaps. Keep the old pilot paused and new intake nondispatchable until reconciliation finishes.

## 6. Model selection, policy and first landing

Model, backend and reasoning choices live in the Hub's organization settings, with optional project overrides, through `/api/v2/organizations/{org}/model-selection` and the project equivalent (see [model selection](hub-self-hosting.md#model-selection)). Model keys in `global.yaml` or repository YAML are not used. Read back the effective stage selections before dispatch.

The runner reports the observed repository policy and onboarding status to the Hub. Definitions committed on the bound repository's default-branch history apply automatically; other descriptors are approved through the project policy API/MCP owner described in [approved repository policy](hub-api.md#approved-repository-policy). Approval does not bypass review or the gate.

Imported bodies must satisfy the approved project's issue contract before dispatch; complete missing sections through the operator API while preserving imported identity and provenance. Moving one native issue to the dispatchable `Todo` lane exercises the attempt, Change version, review, security audit, gate and landing records end to end. An old in-flight attempt cannot be replayed on the new Hub, and the same issue must not be operated from both the old board and the Hub.

Require a successful landing on the intended target branch and the expected terminal state before enabling imported work. Confirm authenticated reads survive a Hub restart, the intended runner has valid credentials and matching policy/model selections, and a fresh backup verifies. Repeat the inventory, import, reconciliation and single-issue verification for each remaining project before retiring the old board. Keep its pinned recovery bundle and stores for the retention window.

## 7. Rollback

A database restore cannot undo pending external effects, commits or GitHub PRs already created. Rolling back stops new dispatch on the Hub through its project controls, lets active work settle, stops the runner, and restores the old board's saved configuration, repository definitions, service file and matching pinned binary. Preserve newly landed commits and reconcile their old issues before resuming. Keep the Hub pilot nondispatchable or its runner stopped while restoring the old owner. Databases are restored only from their coherent stopped-owner backup into an isolated destination first. A restore of a Hub snapshot revokes copied credentials, so runners re-enroll; see [restore, import and move between Hubs](hub-self-hosting.md#restoreimport-and-move-between-hubs).

## GitHub-compatible projects and native cutover

Hub projects have exactly one authority profile. Repository aliases created from GitHub-tracked boards are `github_compatible`; newly created projects are `native`. `GET /api/v2/organizations/{organization}/projects/{project}/integration` returns the profile, configuration revision, repository binding and field-authority map.

| Field or operation | github_compatible | native |
| --- | --- | --- |
| Issue title and body | GitHub | Detent; linked source fields follow GitHub until edited in Cloud |
| Labels, assignees, discussion | GitHub | Detent; GitHub source comments append with provenance |
| Workflow and priority | GitHub projection with existing managed writes | Detent |
| Dependencies | Existing compatibility graph | Detent graph, initially imported at cutover |
| Original authors and timestamps | Source provenance | Retained source provenance plus authenticated native actors |
| Scheduling, leases, run progress | Detent | Detent |
| Repository execution and merge policy | Trusted repository revision | Same approved policy, retained at cutover |
| GitHub PRs, CI, branch protection and required review | GitHub | GitHub when repository integration is enabled |
| Native approval | Detent only | Detent only; never a required GitHub review |

### Integration settings

An administrator updates settings with `PUT /integration` using `idempotency_key`, `expected_revision` (a decimal string), `intake` (`disabled` or `manual`), `projection` (`disabled` or `summary`) and `repository_enabled` (boolean). Bound native projects always report `intake: automatic`; omitting `intake` preserves the stored value. Compatibility projects cannot enable native summary projection. Profile changes happen only through cutover. Disabling repository integration does not authorize bypassing GitHub merge gates.

`detent hub serve --github-disabled` runs a Hub with no GitHub credential; pure native projects need no GitHub repository. `POST /integration/repository` with `repository: "owner/name"`, `idempotency_key` and `expected_revision` attaches a repository to an existing native project, verifying repository metadata without fetching issues. A repository belongs to at most one project and bindings are immutable; a repository already bound to a compatibility project is migrated by importing and cutting over that project.

### Native GitHub intake

A native project with an attached repository and the Detent Cloud GitHub App installed receives new GitHub issues through the App webhook into `Triage`, from any author, with a linked-source snapshot. GitHub authors remain provenance without Cloud membership. `POST /work-items` with `github_issue_url` and `idempotency_key` links an existing GitHub issue the same way; it starts in `Triage` regardless of the requested state. Repeated links and webhook deliveries reuse the source key and native identity.

Later GitHub comments append with provenance, and GitHub title/body updates follow the source until edited in Cloud. GitHub close/reopen events add a Cloud comment without changing state. Cloud discussion stays private. Intake records the link without posting to GitHub; moving an item to `Cancelled` closes the source as `not_planned` with a short link comment, and `Done` closes it only through the landing PR's `Closes owner/repo#N` reference.

The Cloud entry verifies each GitHub webhook signature once at `/api/v1/webhooks/github` and forwards it only to organizations whose native projects bind that repository; a self-hosted Hub verifies signatures on its own origin. App credentials never enter issue or agent context.

### Import and inspect

Historical import uses scoped v2 paths relative to the project URL and requires manual intake on a compatibility project:

1. `POST /imports` with `{"idempotency_key":"import-123","issue_number":123}` returns a durable `import_id`; starting the same source issue returns the existing job.
2. `POST /imports/{import_id}/advance` with the returned `expected_revision` retrieves one page per call and returns the next revision. `GET /imports/{import_id}` reads the job after a lost reply. Network failures keep the cursor and return `status: partial` with a retry time.
3. Traversal covers the issue body, comments, timeline, blocked-by dependencies and GraphQL edit histories. `stage: finished` and `status: retrieved` describe API traversal, not complete history; `gaps` lists what was not retrieved.
4. `GET /imports/{import_id}/records?limit=100` pages through retained source records via `next_cursor`. Missing authors and dates stay unavailable rather than being attributed to the importing actor.
5. Dependency targets must be in the same repository-backed project; cross-project references prevent cutover.

GitHub does not return deleted comments, inaccessible private activity or redacted edit diffs, and pagination is not an atomic snapshot. Reimport uses a new idempotency key with `restart: true` and the job's `expected_revision`; it adds newly discovered source events without overwriting native edits or reinstating removed native dependencies. Newly imported issues are not claimable until traversal finishes and imported dependencies resolve.

### Cutover

Cutover requires no active leases and no processing GitHub writes. `POST /integration/cutover` first runs as a preview:

```json
{
  "idempotency_key": "cutover-preview-1",
  "dry_run": true,
  "initial_state": "Todo",
  "closed_state": "Done",
  "states": [
    {"name":"Todo","dispatchable":true,"transitions":["In Progress"]},
    {"name":"In Progress","dispatchable":true,"transitions":["Todo","Done"]},
    {"name":"Done","terminal":true,"transitions":["Todo"]}
  ],
  "accept_partial": false,
  "close_source": false
}
```

`closed_state` must name a terminal state. The preview returns blockers, incomplete imports, unresolved dependencies, known history limitations and a `checkpoint`, and changes nothing. Applying uses a new idempotency key, the same options, `dry_run: false` and that checkpoint; concurrent source, import or configuration changes invalidate it. `accept_partial: true` acknowledges incomplete retrieval but missing imports and unresolved dependencies still block cutover. `GET /integration/cutover` returns the durable receipt. Approved repository gate/merge descriptors keep their identity under the native scope. `close_source: true` with an absolute `destination_url` enqueues a Detent summary and closes the source issue through the outbox.

After cutover, issue creation, edits, comments, transitions, claims and run events stay in the Hub; issue webhooks and repair cannot overwrite native fields. The `detent hub issue` subcommands `get`, `create`, `edit`, `comment`, `edit-comment`, `transition`, `dependency`, `comments` and `history` call the scoped native APIs, reading mutations as v2 JSON on stdin. With `projection: summary`, `POST /work-items/{work_item_id}/projection` with `idempotency_key` and `body` publishes an explicit summary comment to GitHub. `GET /api/v2/github/requests` returns process-lifetime GitHub request and error counts for administrators.

There is no automatic reverse authority switch or schema downgrade, and a GitHub mirror cannot represent native IDs, actors, revisions, policy, leases or events. Restoring a pre-cutover Hub backup discards later native work and does not reopen source issues or undo external comments. Issue detail, comments, history, revisions and import records remain exportable through v2. Mixed profiles can coexist on one Hub.
