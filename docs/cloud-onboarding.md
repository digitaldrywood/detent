# Project onboarding

The shared product journey starts at `https://cloud.detent.build`: sign in, create
or join an organization, then open a project explicitly shared with you. The
organization chooser, creation/provisioning state, scoped Work/project navigation
and billing reuse the existing Templ/HTMX shell. There is no forced tenant-domain
navigation or persistent global billing banner. Preserve compact/cozy and narrow
layouts, keyboard access, contextual errors and the tab's current run selection.

The organization lifecycle and acceptance trace below are the reviewed-design
target for #2341/#2342/#2343, not shipped self-service behavior. Today's hosted
implementation starts with a reserved organization; its project/runner setup
is reused. Organization administrators create projects; collaboration and runner
management remain separate explicit grants. Retrying project creation with the
same name and creator's write grant resumes that project. Return to its scoped
page to resume setup; infrastructure errors do not require another organization
or host identity.

A new hosted project starts with the workflow `Todo` → `In Progress` →
`Merging` → `Done`, with `Blocked` and `Human Review` beside it. `Todo`, `In Progress` and
`Merging` dispatch work; `Blocked` and `Human Review` neither dispatch nor end it. By
default `review.human` is false, so nobody has to review a run. A run
that committed a change publishes a version the policy already accepts, and the
runner's orchestrator moves it straight to `Merging`, where the runner that
holds the project lands the head on the base branch with plain git and the Hub
finishes the issue in `Done`. A run that committed nothing moves to `Done`.

`Human Review` is where a Change Request waits for a person only when the
project sets `review.human: true`. With the default `false`, an unaccepted
version or refused landing moves to `Blocked` with its reason. A person approves the
change in the client, which moves it to `Merging`, or requests changes, which
sends it back to `In Progress`. `Human Review` is the default
`auto_promote.source_state`.

Hub migrations 36, 38, 39 and 41 move projects whose workflow is exactly an earlier
template onto this one. A customized workflow is left unchanged; it needs a
`Blocked` lane reachable from `In Progress` and `Merging` when `review.human` is
false, or a review lane matching `auto_promote.source_state` when it is true.
Accepted changes need a dispatchable `Merging` lane reachable from `In Progress`.
Hub migration 40 gives every project with an approved repository policy a
default review policy; approving the policy again now seeds `require_review`
from `review.human`.

The hosted hostname migration is tracked in the [Cloud domain runbook](cloud-domain-migration.md).
Its provisioning results and pending browser cutover are recorded there.

## Runner access through Luna

In **Add runner > AI assisted**, an own-machine request such as “add a runner
on my laptop” asks once whether agents should run in the sandbox or with full
access. Sandbox is the default for a shared machine; `native-trusted` is
available for a dedicated machine. Sandbox restricts agent access, while full
access lets agents use the machine's files, credentials and network. Luna
carries the answer into the enrollment `isolation_tier` field and the
`detent hub runner register --isolation-tier` command. Obtain enrollment tokens
through the secure enrollment dialog, never through chat.

Fly Sprites are dedicated VMs. Luna defaults to full access inside each VM and
lets you select sandbox when configuring the pool. See [Sprite runners](sprite-runners.md).

For an existing runner, Luna reads its selected tier, backend support and
current problems. `tier_unavailable` means its agent backends do not support
the selected tier. An organization owner or admin can approve Luna's preview
to switch to a supported tier without enrolling again. Approval is always
required for runner access changes, even with ordinary chat confirmation off;
non-admin members cannot make this change. Projects requiring sandbox remain
ineligible for a `native-trusted` runner. Luna reads the runner again after the
change to verify the tier and identify any remaining problem.

## Organization provisioning and recovery

Implemented by `detent cloud serve` when its configuration has an `allocation`
block (see [shared entry](examples/hub/README.md#self-service-provisioning)). Without
that block the shared entry offers no creation and routes only registered tenants.
The entry keeps each intent in its registry keyed by the verified subject and a
per-form creation key, and advances it through the checkpoints below with bounded
retries (`retry_limit`, exponential backoff up to five minutes). WorkOS creation is
recovered by the organization's external ID, owner membership is looked up before it
is created, the tenant database is initialized by the tenant process itself, and
the tenant accepts the owner only after the provider reports that exact subject as
an active owner. A restarted entry resumes pending intents and relaunches ready
tenants. The shared web client screens for these states follow the
web/conversation client; the entry currently serves minimal pages in the existing
shell (`/organizations/new`, `/organizations/ORG/provisioning`,
`/organizations/ORG/delete`) and JSON at `/api/cloud/organizations/ORG/provisioning`.

A verified signed-in identity submits `POST /organizations` with a session-bound
CSRF token and stable creation idempotency key. Persist a fingerprint of the
validated creation input and an opaque organization ID before external effects.
Same user/key/payload returns the same operation; a changed payload conflicts.
Separate tabs retrying the same intent cannot acquire duplicate owners or slots.
An organization display name is never identity or proof of ownership.

| Durable checkpoint | Required action and recovery invariant |
| --- | --- |
| `requested` | Bind creator to verified provider issuer/subject and creation intent, enforce eligibility and per-identity quotas. Unverified email, client-submitted owner IDs and support impersonation cannot bootstrap ownership |
| `allocating`: reservation | Atomically reserve bounded process/memory/disk capacity with an allocation generation and fenced worker lease before creating resources |
| `allocating`: provider | Create/recover the WorkOS organization and owner membership for that exact subject; persist provider IDs and step result. Query/reconcile an uncertain response before another create |
| `allocating`: tenant | Allocate a private path and service identity from trusted IDs, initialize a fresh tenant database through its sole owner, persist immutable organization/provider/generation binding and that creator's owner membership |
| `allocating`: free access | Assign the configured versioned free plan idempotently. No Stripe customer, card or paid subscription is required |
| `ready` | Publish the registry route only after database health, tenant/provider membership, allocation and plan references agree. Link to `/organizations/ORG/work` and existing project/runner setup |
| `failed` | Preserve intent, reservation/resource inventory, completed checkpoints, bounded error and retry eligibility. Offer Resume for recoverable failures, without generating a new identity. No capacity shows a retryable state; existing tenants stay healthy |
| `deleting` / `deleted` | Apply the RFC owner-confirmed retirement contract, fence routing/writers and track erasure/retention results. Tombstones prevent reuse and resurrection |

A provider create without a usable idempotency/reconciliation mechanism cannot
be blindly retried after an uncertain response. Record it as unresolved and
reconcile through the provider's supported lookup or operator repair of that
same intent. Likewise reconcile live process ownership before replacement.
Retry workers have finite attempt/deadline/backoff limits; exhaustion exposes a
sanitized resumable failure and retains evidence/resources safely. Cleanup uses
an explicit allocation inventory and ownership checks, never recursive deletion
of an inferred user path. Do not erase customer data after a failed signup.

The verified subject can recover a pending intent after login or process restart.
Provider email changes do not transfer ownership. An existing organization must
be joined through current membership or a provider-validated exact-recipient
invitation, never claimed by matching its name/domain or replaying a bootstrap
subject. Membership removal, re-invitation and last-owner protections remain in
[hosted identity](hosted-identity.md#permissions-and-revocation). Account and
organization deletion follow the [RFC lifecycle contract](cloud-hub-rfc.md#provisioning-recovery-and-capacity).

## Repository configuration and policy

Keep the customer checkout and its `detent.yaml` / `WORKFLOW.md` on the customer
host. Inspect existing files before using `detent onboarding draft-answers`,
`explain-answers`, or `build-workflow`. The builder's `--help` describes its local
output options. Review generated files before applying them. Cloud does not write
repository files. A supplied repository definition owns the native workflow;
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
both authorized issue moves and execution progress. Include completion and
landing transitions when constraining execution states.

The runner includes bounded state and transition metadata in its repository
policy descriptor and reports unapproved revisions through the existing policy
observation path. An authorized owner/admin approves the exact descriptor with
the current expected policy identity. Approval atomically persists the native
workflow, repository source and revision alongside the policy. Active leases
retain their approval. States occupied by issues, including archived issues,
cannot be removed; move those issues through ordinary authorized transitions
before removing a state. Approval never remaps work items. Conflicting runner
reports remain separate candidates for explicit approval. An existing runtime
applies a changed policy through its settled, paused configuration owner, or
loads the approved definition on restart.

Cloud settings show the approved repository source and revision and direct
definition edits to the repository. UI, API and MCP definition writes cannot
override repository authority. Authorized issue moves and other integration
settings remain available. Older approvals without workflow metadata retain
their states until an upgraded runner's descriptor is explicitly approved;
an old descriptor cannot replace an already repository-controlled workflow.

With no supplied repository definition, the Workflow settings editor accepts
Markdown using the same legacy frontmatter schema: `tracker.kind: hub_native`,
`tracker.active_states`, `tracker.observed_states`, `tracker.terminal_states`,
and optional `server.kanban.allowed_transitions`. The body contains agent
instructions. Cloud persists that Markdown and derives its lanes with the
existing parser. Runners load it through the existing definition and policy
approval path; execution changes still require approval. A saved edit makes
the prior definition digest stale: claims and approvals must match the current
Markdown revision. Approval history remains intact. Adding a repository
definition transfers authority through that approval path without losing work
items. Repository-controlled definitions cannot be replaced by Cloud Markdown
or independent state arrays.

For the shared-site target, use `client.hub_url: https://cloud.detent.build`
and the selected immutable organization ID. Configure `client.organization_id` and
`client.native_projects` mapping in the instance configuration. The project's
page shows its native ID. Run `detent doctor` against that configuration; resolve
reported problems locally. Sign in to the selected agent provider on that host.
Only checkbox acknowledgments of these local checks are saved in Cloud; they are
explicitly user-reported and do not override runtime validation.

Run `detent hub policy inspect --config /path/to/config.yaml --project LOCAL_PROJECT`.
Paste only the resulting descriptor into the policy approval form. The form
accepts validated, bounded policy metadata, including source revision, digests
and the resolved native workflow projection, rather than arbitrary runner
configuration. An organization owner/admin with project write
access approves it explicitly. Approval retains the existing compare-and-swap
and active-lease guards. Edit gate, review, auto-promotion, merge and runner
requirements in the repository, then approve the new descriptor. A running
runner reports the policy it resolved: Settings, Projects, then the project's
Settings shows "A runner is waiting for a new policy" with an "Approve reported
policy" button, so pasting the inspect output is only needed before any runner
has started.

A native project (`tracker.kind: hub_native`) makes no GitHub REST or GraphQL
calls by default. The runner does not give the agent the instance GitHub
credential, open or look up pull requests, or ask the agent for Workpad comments;
it opens the native Change Request from the attempt's commits after a successful
run, and the local gate is the CI. Git transport (clone, fetch) still works. The
agent prompt ends with a native completion contract that overrides GitHub steps
in `WORKFLOW.md`. `detent doctor` and `detent hub policy inspect` warn when the
repository `WORKFLOW.md` still describes GitHub steps; remove them from a
native project's workflow. Setting `worker.github_token` explicitly in the
project is the only way to give its workers GitHub access.

## Customer host enrollment

In the organization's runner settings, choose Enroll a runner, give the runner
a name, pick its projects, and copy the one command the dialog shows. Run it on
the host:

```sh
detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID \
  --token TOKEN --name "Build host" --service
```

The command generates the host's identity locally, redeems the one-time token,
writes `~/.config/detent-runner/global.yaml` and `identity.json`, and installs
the `detent.runner` background service. Clone each project's repository into
the directory it prints (`~/detent-runner/PROJECT` by default) first, or run the
`detent start --config ... --yes` command it prints after cloning. Nobody copies
a runner or machine ID by hand. See `docs/hub-api.md` "Register a runner with
one command" for what each step writes.

Enrolling needs runner management on every organization project. An owner or
administrator who creates a project receives it automatically; grants for other
members are set in Organization.

Retry `register` with the same configuration directory: it reuses the identity
it generated, never creates a second runner, and never rewrites an existing
`global.yaml`. Do not copy an enrolled identity file to another machine. The
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
Sprite is supported; do not copy its identity to another Sprite.

The script ends with `sprite-env checkpoints create` and prints the ID; restore
the same Sprite with `sprite-env checkpoints restore CHECKPOINT_ID`. A failed
checkpoint fails the script but leaves the enrollment and service in place.

Remaining manual steps, which the script never performs:

- Sign in to the providers the project uses: `claude auth login`, `codex login`
  (`codex login --device-auth` without a browser), or a Sprites provider
  connector.
- For private clones and pushes, run `gh auth login` and `gh auth setup-git`,
  or set up the project's own git credentials, and set the git author.
- Clone each project into the directory `register` printed, with its
  `WORKFLOW.md` and `detent.yaml`, and install its dependencies.
- Approve the project's observed repository policy in the Hub if it is pending.
- Re-run the script with empty input, check
  `sprite-env services get detent-runner` and the Hub's runner health, then
  route a Todo issue and confirm it pushes a branch.
- Take a new checkpoint so the baseline includes the sign-ins and checkouts.

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

## Artifact history and GitHub

Preserve the September 7 choices: local-only history, customer-managed durable
storage, and explicit opt-in hosted storage through the portable S3 adapter
(DigitalOcean Spaces initially). No setup default silently uploads artifacts.
Local history keeps artifacts on your own runner without a separate storage
account or fee; losing that machine loses its artifacts. Customer-managed
history uses an S3-compatible bucket and artifact gateway that you run and pay
for. Detent records the binding only and stores neither your artifacts nor your
storage credentials. For history with execution runners offline, follow
[artifact deployment](artifacts-deployment.md):
private S3-compatible bucket, durable catalog, separate TLS gateway, dedicated
publisher identity, and local storage credentials. Register only the gateway
origin, service ID and publisher token ID. Never enter the publisher token secret
or storage credentials in the hosted form. Run `verify-storage`, then test opening
a retained artifact with execution runners stopped. A registered binding is
configuration evidence, not a storage-health or offline-availability guarantee.

Native issue creation works with GitHub disabled. To connect GitHub:

1. Associate the repository as `owner/name` in Project setup after cloning it on
   an enrolled runner.
2. [Install the Detent Cloud GitHub App](https://github.com/apps/detent-cloud/installations/new)
   on the repository or organization. Choose all repositories or only selected
   repositories, including this project's repository. The App requests Issues
   and Pull requests read/write, and Contents, Checks, Commit statuses and
   Metadata read permissions.
3. Open Project setup to read whether Detent Cloud is installed on `owner/name`.
   New GitHub issues enter Triage automatically once the App is installed.

If the deployment has GitHub transport, attach a repository before manual imports, summary
projection or repository/PR integration. Each choice is separate; saving setup
progress does not activate an integration. Existing profile, repository binding,
idle-work and revision checks still apply. Import execution and summary publication
remain separate actions through the existing native API.

## Shared protocol and recovery

Self-hosted Hub uses the same scoped API with customer-selected authentication,
without a Detent Cloud account, subscription or Stripe connection. These are
currently supported project setup endpoints:

- `GET /api/v2/organizations/ORG/projects/PROJECT/onboarding` reads persisted
  progress, current approved policy, eligible runners, bindings and latest run.
- `PUT` to the same path accepts `idempotency_key` and `progress`, containing
  string `revision`, `repository` (`existing` / `generate`), booleans `doctor`
  and `provider`, and `artifacts` (`local` / `customer`).
- Writes compare revisions and reuse the native idempotency journal. Restarting
  the service retains progress. Browser retries retain a command key and payload
  fingerprint in session storage; no issue body or provider credential is stored
  there. In the shared-site target, browser retry keys also bind organization,
  project and user so concurrent tabs cannot replay into another scope.
- The `/onboarding/policy`, `/onboarding/artifact-services/SERVICE`,
  `/onboarding/integration` and `/onboarding/repository` adapters require scoped
  administration and call the existing validated implementations.

A stale revision returns 409 and requires rereading current state. Invalid input
returns 422, access failures require account/grant correction, and service failures
require retrying the existing operation. The first native issue uses the ordinary
idempotent work-item API. Its state determines whether it queues; policy and runner
checks remain authoritative for execution.

The current `artifacts` progress field accepts only `local`/`customer`; the
approved opt-in hosted mode is a target storage choice, not a new value accepted
by this API today. Future setup must save explicit custody consent and use the
configured portable artifact service rather than overloading local/customer.

## Shared-site acceptance trace

This is a test specification for #2341/#2342/#2343 and the expanded #2199 evidence,
not a claim that current reserved-tenant fixtures pass it. Use synthetic providers,
ephemeral services and one test hostname representing `cloud.detent.build`.

Alice and Bob are unrelated verified identities. Alice creates A, Bob creates B,
and Casey receives separate invitations to both. A has project PA and B has PB.
Each tenant owns a separate database and process. Every application URL below is
on **https://cloud.detent.build**; WorkOS/Stripe hosted screens may temporarily leave
the site only for their authenticated provider flow and return to that same host.

| Step | Action and expected evidence |
| --- | --- |
| Signup | Alice and Bob create A/B from `/organizations/new` without operator YAML, bootstrap user, DNS or public port allocation. Crash/retry after every checkpoint returns the original A/B IDs with exactly one verified owner each |
| Capacity | Concurrent creates at the admitted limit leave a resumable capacity failure; already-ready A/B remain routable, no extra process or unsafe disk allocation appears |
| Invitations | A/B admins invite Casey through the common `/invite` entry. Exact recipient and provider organization must match; wrong-account, alias, expired/replayed and guessed-organization attempts fail. Casey sees both memberships, never an unrelated organization |
| Grants | Grant Casey read-only PA and write PB, separately from runner management. Ownership/admin status alone never reveals project content. Bob cannot read PA by guessing its ID; cross-tenant searches/cursors return no A content |
| Concurrent tabs | Casey opens `/organizations/A/projects/PA/...` in one tab and `/organizations/B/projects/PB/...` in another. Selecting a B run, submitting a B form, reauthenticating B or opening billing leaves A's URL, CSRF context, stream and run/attempt selection intact |
| Mutations and streams | A write remains denied and B write commits only to B. Swap route/body/header IDs, reuse a CSRF token in the other scope, spoof proxy authority, and bypass the entry service: all fail. Frames/cursors from A never appear in B |
| Billing | Free signup creates zero Stripe customers. Alice's first paid action creates/reuses A's test customer; Bob's creates/reuses B's. Casey cannot purchase as member. Swapped customer IDs, A return URLs in B and cross-account/mode webhooks cannot grant or expose another organization's plan/invoices |
| Runners | Enroll named/tagged RA for A/PA and RB for B/PB against the same base URL. Each renews/posts events/claims only its own scope; cookie changes and forged IDs cannot move credentials. Exact-host no-match waits without fallback |
| Artifacts | Test local-only without uploads, customer-managed durable service, and explicit opt-in hosted Spaces-compatible fixtures. An A grant fails for B, expired/revoked grants fail, and retained uploaded artifacts remain readable with execution runners stopped. Local-only makes no offline-availability promise |
| Review policy | PA requires human review; PB permits automatic merge after checks. Repo-local policy remains authoritative for both, with external branch protections and independent-check principals preserved |
| Revocation | Remove Casey from A while both streams are idle: A protected requests/download authorization fail immediately on recheck and its stream closes within 30 seconds. B stays usable. Shared logout/expiry closes both scopes |
| Restore/deletion | Restore paired registry/tenant fixtures with current tombstones and a new generation; stale authority fails. A owner deletion remains resumable until erasure completes, does not affect B, and is never reversed by old snapshots/webhooks. Casey account deletion removes both memberships without deleting Alice/Bob's work |
| Free self-hosting | With custom/local auth, block all Detent Cloud, WorkOS and Stripe networking. Create native work, enroll a runner, execute/checkpoint, export and recover with no subscription or hosted entitlement requirement. Repeat using a local WorkOS fixture with Cloud/Stripe blocked: auth selection still performs no billing call |

Record browser evidence for the existing shell in compact/cozy and narrow layouts,
no-access/expired-session states, provisioning interruption and the two-tab trace.
Record tenant database IDs and bounded routing/provider call counts without
content or credentials. #2199's free pilot checks free/complimentary billing UI;
paid customer/webhook cases are #2343 fixtures and do not block that free pilot.
The retained single-tenant evidence is a baseline, not shared-site launch evidence.

## Independent Change checks

Known setup limitation: the private non-hosted maintenance recipe below is not
executable for an already bound hosted database, which rejects local-mode reopen.
[#2350](https://github.com/digitaldrywood/detent/issues/2350) tracks a supported
credential-maintenance path and correction of that recipe. Keep the binding
checks intact. The existing scoped independent-check submission protocol remains
valid; this RFC does not add token provisioning/rotation/revocation authority.

Hosted admission permits a dedicated `operator` bearer to submit only
`POST /api/v2/organizations/ORG/projects/PROJECT/work-items/ITEM/changes/CHANGE/versions/VERSION/checks`.
The credential must be active, belong to the hosted organization through an
explicit current project grant, and be named by an `independent` required check
in that project's current approved Change review policy. An administrator bearer,
execution worker, hosted user cookie, or unapproved operator cannot substitute.
This exception grants no reads, publication, review decisions, policy changes,
imports, or token administration.

Enrolled runners retain their existing `collaborate` operation for expected
`customer` checks. They cannot satisfy an `independent` check or change the
source pinned by the immutable version.

Provision and maintain credentials for an already initialized hosted tenant during
an explicitly authorized private maintenance window. Stop the hosted process,
then start the sole database owner with the **same hosted configuration**, database
path, organization/provider/bootstrap identity and public URL. Set
`workos_organization_id` explicitly to the allocated provider organization
if the original bootstrap configuration omitted it:

```sh
detent hub serve --database /var/lib/detent/hub.db \
  --hosted-config /etc/detent/hosted.yaml \
  --credential-maintenance --listen 127.0.0.1:7778
```

Keep this listener private, outside all reverse-proxy routes and port forwarding.
Maintenance rejects non-loopback listeners and `--trusted-proxy`. Each request
below goes to `http://127.0.0.1:7778` with
`Authorization: Bearer <existing-instance-administrator-secret>`. Loopback
alone provides no authority. Use the existing unscoped instance administrator
credential retained at initial setup; the default `DETENT_HUB_ADMIN_TOKEN`
environment variable is still required by the serve command, but maintenance
never creates or resets a bootstrap credential. Browser cookies, staff sessions,
scoped administrators, CI reporters and runners cannot authorize maintenance.

Only the four token creation, rotation, revocation and project-grant routes are
served. Requests record the administrator principal in hosted audit history.
The normal database ownership lock prevents a second writer. Tenant binding
checks remain enabled; ordinary non-hosted reopen still fails. Shut down
maintenance before restoring the original hosted command. Public hosted serving
continues to deny instance token administration.

1. Create a dedicated token with `POST /api/v1/tokens`, using
   `{"name":"project-independent-ci","scope":"operator"}`. Deliver the returned
   secret once through the CI secret store; retain its public `id` for policy.
2. Grant exactly the intended project with `POST /api/v2/tokens/ID/grants`, using
   `{"organization_id":"ORG","project_id":"PROJECT"}`. This also marks the
   token native-only. Use a separate principal per project when independent
   revocation is needed. Restore hosted serving after maintenance.
3. An organization owner/admin with project write access approves
   `PUT /api/v2/organizations/ORG/projects/PROJECT/change-review-policy` using
   their hosted session and CSRF token. Include `idempotency_key`, the current
   `expected_review_policy_id` when replacing a policy, and `policy` containing
   the approved repository `policy_id`, `require_review`, and `required_checks`.
   Each check pins `name`, the credential ID as `principal_id`, `workflow_id`,
   `workflow_sha256`, `source: "independent"`, and `max_age_seconds` (60–604800).
   Repository review and check floors still apply.
4. Publish a version, then deliver its exact check expectation and immutable
   head, run, policy/configuration and workflow identities to the CI job through
   the publishing integration. The result bearer cannot fetch them itself.
   Submit a terminal result and evidence references with a unique idempotency
   key to the exact version's `/checks` route.

To revoke submission access immediately, the hosted project administrator
replaces the approved review policy with an eligible replacement CI principal,
using its current policy identity. The removed principal cannot submit or replay
results, even for old versions. Publish a new version after policy replacement;
old versions retain their immutable policy and display stale-policy readiness.
For permanent credential revocation use `DELETE /api/v1/tokens/ID` through the
private maintenance administration path. Rotation uses
`POST /api/v1/tokens/ID/rotate`, invalidates the old secret, and preserves the
principal ID and grants; distribute the replacement only to the authorized CI.
A removed project grant also immediately denies submissions.

Credential activity, hash, expiry, scope, project grant and current policy pin
are rechecked within the mutation transaction before returning cached results.
Results still validate the version's expected principal, head/run, policy/config,
check-run ID, workflow digest, independent source and completion timestamp.
Changed terminal results conflict; retries cannot approve another version.
Evidence loses readiness at its freshness bound. Human-review repositories also
need current-version human approval; automatic repositories need passing checks.
Native `reviewed` status preserves the separate GitHub branch-protection gate and
does not authorize an external merge.
