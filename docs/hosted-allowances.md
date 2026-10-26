# Hosted capacity allowances

Organization entitlements are enabled only when a plan catalog or legacy plan
metadata is configured. By default, local and self-hosted Hubs, including Hubs
using `--hosted-config` for hosted identity and the web app, do not create plan
assignments or apply plan allowances. Omitted or empty `entitlements` leaves
plans disabled, just like a bearer-only Hub. Runner, host, policy and provider
safety limits still apply. With the Cloud capacity catalog configured, no Stripe
account, subscription, or card is necessary for the permanent Free base plan.
Model-provider charges remain separate from hosting.

## Capacity catalog

| Plan | Version | USD per organization/month | Concurrent work |
| --- | ---: | ---: | ---: |
| Free | 1 | 0 | 2 |
| Starter | 1 | 49 | 8 |
| Team | 2 | 99 | 20 |
| Growth | 2 | 249 | 50 |
| Scale | 2 | Contact us | 100, or custom by grant |

The Cloud capacity catalog applies only when explicitly configured in
`entitlements`; its base assignment is `free` version 1. `team` uses version 2
because version 1 is a legacy paid mapping. Growth and Scale moved to version 2
with the new prices; their version-1 records keep their stored prices for
existing subscriptions and resolve with Growth and Scale concurrent work. Scale
has no self-serve price; operators grant it, or a custom plan, as complimentary
access. Free signup needs no card, Stripe customer, or subscription.

Concurrent work counts the organization's live claims: every running attempt,
whatever its stage. A claim at the limit is refused with `plan_capacity`; the
runner treats it like full capacity, the item stays in its lane, and it becomes
claimable when a slot frees. Nothing is refused at filing time. Every tier,
Free included, has native execution; paid tiers add hosted artifacts. No tier
limits projects, members, repositories or runners.

Issues keep an abuse guard of 10,000 open issues across the organization,
reported as `unarchived_issues`. Issues in a terminal lane (Done, Cancelled) and
archived issues do not count. Issue creation, imports, restore and moves out of
a terminal lane use the existing atomic mutation transaction. Luna chat and
hosted AI are gated by AI credits and the monthly budget, not by tier. Runner,
host, policy and provider safety limits continue to apply independently.

Capacity tier records resolve their concurrent-work allowance, open-issue guard,
execution feature and unlimited projects from this catalog when read, so
existing assignments and grants follow it without new versions; their stored
names and prices remain authoritative. A grant of a capacity tier carries that
tier's concurrent work and open-issue guard whatever its recorded scope.
Legacy `pilot_free` and `comp_team` have no concurrent-work or project limit
once the capacity catalog is installed. In any other plan, a configured
`concurrent_work` is enforced and an omitted one is unlimited.

## Configuration and plan versions

An explicit catalog enables plan allowances and must retain any referenced legacy
versions. The following is the legacy pilot catalog, retained for compatibility;
Cloud deployments configure the capacity catalog instead:

```yaml
entitlements:
  base: {id: pilot_free, version: 1}
  window_seconds: 3600
  retention_windows: 24
  connected_seconds: 90
  invitation_seconds: 86400
  plans:
    - id: pilot_free
      version: 1
      features: [collaboration, native_execution, github_integration]
      allowances:
        members: 10
        projects: 10
        repositories: 10
        registered_runners: 10
        connected_runners: 10
        concurrent_work: 5
        api_mutations: 10000
        ingested_events: 10000
        collaboration_bytes: 67108864
        history_records: 10000
    - id: comp_team
      version: 1
      features: [collaboration, native_execution, github_integration, hosted_artifacts]
      allowances:
        members: 20
        projects: 20
        repositories: 20
        registered_runners: 20
        connected_runners: 20
        concurrent_work: 10
        api_mutations: 20000
        ingested_events: 20000
        collaboration_bytes: 134217728
        history_records: 20000
```

Omitting the entire section, or leaving it empty (`entitlements: {}`), applies no
plan allowances. Any key inside the section, even `plans: []`, makes it an
explicit catalog that must validate. With the pilot catalog above configured,
every new organization starts on `pilot_free`, and `comp_team` exists only to be
granted as complimentary access. `comp_team` doubles the `pilot_free` allowances
and adds `hosted_artifacts`; its artifact allowances stay zero until an operator
configures them (see below).
Within an explicit plan, omitted allowances other than `concurrent_work` are
zero and omitted features are disabled. Negative values, unknown names and unsupported configuration bounds
are rejected. Windows are whole multiples of 60 seconds, at most one day;
telemetry retains at most 720 windows and 30 days. Invitation reservations last
between one minute and seven days. These are validation bounds, not public
retention periods.

Plan records are immutable by `(id, version)`. To change a plan, add another
version and explicitly assign it. Configured `base` initializes a new assignment;
restarting or changing the file does not overwrite an existing assignment or a
later administrator decision. Existing plan versions remain available for audit
and historical grants. Legacy `plan_id`, positive `storage_quota_bytes`, and
positive `event_quota` configure the initial pilot plan when `entitlements` is
absent; they cannot be mixed with the new section.

## Administration and precedence

Configure a separate secret for entitlement administration:

```yaml
entitlement_administrator: operator_example
entitlement_admin_token_env: DETENT_ENTITLEMENT_ADMIN_TOKEN
```

The environment variable must contain at least 32 bytes of independently
generated secret material, separate from the metadata reporter token. Ordinary
staff, support impersonation, organization owners/admins and runners cannot
grant complimentary access. This credential changes allowances only; it gives
no customer project access or runner execution permission. Keeping it unconfigured
disables remote plan mutations while allowing free access and usage reporting.

The authenticated operator posts to
`/api/v2/organizations/{organization}/entitlements`. Each command requires a
unique `idempotency_key`, current `expected_revision`, `action`, and bounded
nonempty audit `reason`. The transaction records operator identity, command,
reason, time and request hash. A matching retry changes nothing; a changed
request under an existing key or a stale revision is rejected. Audit records
remain in the owning organization database and never enter routine analytics.

```json
{
  "idempotency_key": "pilot-access-example",
  "expected_revision": 1,
  "action": "grant",
  "grant_id": "pilot_access",
  "plan": {"id": "comp_team", "version": 1},
  "scope": ["projects", "concurrent_work"],
  "reason": "Approved pilot access"
}
```

`GET` on the same path with the same credential returns the organization's base
and effective plan, current revision, persisted plan versions, and every grant with its
recorded reason and administrator identity. No other credential can read it.

A `create_plan` command creates a new immutable version in the catalog without
changing an organization's assignments. It requires a `definition` matching
`plan`, with `features`, `allowances`, and optional display name and monthly USD
cents. Creation advances the entitlement revision and uses the same audit and
idempotency rules as assignments and grants. For example:

```json
{
  "idempotency_key": "create-capacity-example",
  "expected_revision": 1,
  "action": "create_plan",
  "plan": {"id": "capacity_example", "version": 1},
  "definition": {
    "id": "capacity_example",
    "version": 1,
    "name": "Extended capacity",
    "features": ["collaboration"],
    "allowances": {"collaboration_bytes": 17179869184}
  },
  "reason": "Approved capacity increase"
}
```

Read the new revision, then assign or grant the created version. Creating an
existing version under a different command key conflicts; it never overwrites
the stored definition. File plans are startup seeds: matching definitions do
nothing, and differing existing versions warn and retain the stored version.

Grant `starts_at` defaults to transaction time; optional `expires_at` uses an
RFC3339 timestamp after the start. A `revoke` command names `grant_id` and a
reason. `base` assigns a plan version. `subscription` assigns a plan version
with a required future validity deadline; `end_subscription` clears that
derived access when Stripe billing is disabled. With
[hosted subscription billing](#subscriptions-and-ai-credits) configured, authoritative
Stripe reconciliation owns subscription-derived access and these two commands
are rejected. Complimentary grants remain independent of payment records.

### Granting from the platform console

On the shared entry, registry platform members with `admin` or `billing` roles
may create plan versions, set the base plan, and grant or revoke complimentary
plans. Subscription commands remain unavailable when Stripe owns access. Configure
`allocation.entitlement_admin_token_env` so the entry can reach tenant entitlements.
`platform.bootstrap_admin_email` restores the operator as an admin at startup.
The deprecated `entitlement_administrators`, `support_actors`, and `staff_emails`
seed an empty platform membership registry; they do not gate access after seeding.
See the [shared entry example](examples/hub/README.md#shared-entry).

Authorized members can read the base plan, effective plan and active grants, and
copy an existing plan into a new immutable version, edit its features and
allowances, and apply base or grant changes with a required reason. `support` and `viewer`
roles, support impersonation sessions, and non-members cannot change plans.
Customer organization ownership does not grant platform billing access.

The entry reads and changes the plan server-side through the tenant's private
socket, presenting the allocation's entitlement credential; the credential never
reaches the browser. It forwards the console's `idempotency_key` and
`expected_revision`, grants the whole chosen plan (every feature and nonzero
allowance), and records each applied change in the registry's append-only
`entitlement_changes` table with the staff email, organization, plan, expiry and
reason. A stale revision is shown as "someone changed this organization's plan;
reload and try again".

The platform API at
`/api/cloud/platform/organizations/{organization}/entitlements` accepts the same
plan actions, with session CSRF protection or the dedicated entitlement bearer
credential. The platform MCP endpoint `/api/cloud/platform/mcp` exposes
`platform_entitlements` and `platform_change_entitlement` under the same current
administrator authority. Customer API keys do not acquire this authority.

### Reloading Cloud configuration

On Unix, SIGHUP reloads the existing `cloud.yaml` file. The full document,
including restart-only fields, must parse and validate. The reloadable fields
are `allocation.entitlements`, `allocation.billing.prices`,
`allocation.allowed_emails`, and `allocation.allowed_domains`. Existing plan
versions and Stripe price mappings remain immutable; use a new version or price
rather than changing one already stored. The entitlement policy, including
usage-window settings, and the active billing price list publish as one
immutable snapshot in each tenant after the database transaction succeeds.
New plans, reloads, and their reasons are recorded in `hosted_plan_audit`.

The entry validates every ready managed tenant, then prepares immutable plan
records without exposing the new catalog, policy, or active prices. One atomic
publication file selects the prepared settings for all managed tenants and
future launches. Tenant Hubs read this shared publication through an entry-owned
link beside their database; publication requires no tenant activation request.
A rejected preparation, a lost reply after preparation commits, or a persistent
transport outage leaves the previous publication active. Unpublished plan records
cannot be read or assigned through the entitlement API. Preparation and reload
logs retain actor and reason. This replaces tenant-by-tenant application and
compensating restoration. Startup never replaces persisted assignments or grants.

After a successful reload, the entry applies the new sign-up allowlist to the
next organization-creation permission check and the platform Sign-up allowlist
read at `/api/cloud/platform/allowlist`. Empty email and domain lists allow anyone
to create an organization. A failed reload retains the previous allowlist;
existing organizations and sessions are unaffected.

Listeners, origins, database/state paths, signing and provider credentials,
platform membership seeds, attachments, allocation paths and capacity,
entitlement credentials, and billing settings other than prices require a
restart. Reload logs list these fields and retain the running values, including
for future tenant launches. SIGHUP does not restart the entry or tenant Hubs.

Resolution at the allocation transaction's clock sample is:

1. Start with the organization base assignment.
2. A subscription with a validity deadline strictly after now replaces the base.
3. For each active grant, take the maximum allowance for each explicitly scoped
   resource and enable each explicitly scoped feature present in its plan.
   Grants do not add quantities together or enable other features from that plan.
4. At the exact expiry timestamp the grant/subscription is no longer valid.
   Revoked grants have no effect. Stored assignments and customer data remain.
5. Membership, project/runner permissions, policy approvals, repository gates,
   host capacity and provider safety/budget brakes always continue to apply.

## Measurement and exhaustion policy

| Allowance | Consumption and enforcement |
| --- | --- |
| Members | Locally active memberships plus unexpired invitations, measured for operation only. No seat-based plan limit applies. |
| Projects/repositories | Stored allocations in the dedicated tenant Hub. Their creation/binding transaction must fit the allowance. |
| Registered/connected runners | Non-revoked runner registrations, including legacy machine identities without a runner registration; connection means a heartbeat strictly newer than the configured cutoff. New enrollments and reconnecting idle runners cannot grow the count beyond the limit. |
| Concurrent work | Unreleased leases whose expiry is strictly after the transaction clock. A claim beyond the plan's `concurrent_work` is refused with `plan_capacity` and the item stays in its lane. Operational runner and host capacity still apply. |
| API mutations | Accepted allocation-changing transactions per configured UTC window. Matching native command retries do not consume another unit. Reads, billing, renew/release and completion/checkpoint events remain available. |
| Ingested events | Newly stored collaboration events per window. Native retries and ordered duplicate event sequences do not add an event. |
| Collaboration bytes | UTF-8 bytes of issue text/labels/assignees, comments, versions, event payload/actor records other than run telemetry (`run.*`) and scheduler decisions, artifact references, change source bundles, import snapshots and stored attachments. Idempotency receipts, attempt records, run telemetry and scheduler decisions are internal bookkeeping and do not count. This is logical retained payload, not disk billing. Database/WAL bytes are separately reported. |
| History records | Collaboration events, immutable versions and ordered attempt-event receipts. |

Checks and allocation writes use the same transaction. An operation that increases
an exhausted resource is rolled back with HTTP 429, `allowance_exhausted`, the
resource, allowance and prior consumption. Reductions and unchanged resources
remain possible after a downgrade. Data is not automatically deleted to make
an organization fit a smaller plan. Fenced checkpoint and completion events may
grow bounded, validated execution history beyond the tier; ordinary edits do not
receive this exemption. Existing request/payload and lease safety limits still
bound each event. The worker must checkpoint or finish when ordinary mutations
are exhausted. Existing history can still be read and exported; owner billing
and administrator plan pages remain accessible.

Usage is available at `/organization/plan` to owners/admins and through owner-only
`/api/cloud/billing`. The existing staff/reporter metadata endpoint includes the
same bounded entitlement summary. Project access remains separate. No permanent
banner is added. Model-provider spending is not presented as a Detent charge.

## Hosted artifact storage and relay

Customer-mode artifact services never contact the hosted allowance endpoint and
keep their configured local storage policy. Hosted services opt in explicitly,
use the organization-bound publisher credential, and request allowances from the
owning Hub. The first authorized hosted service ID binds the organization's
artifact allowance owner. A second ID is refused; changing owners requires an
operator-controlled migration of the catalog, not another independent service
with the same full quota. Run exactly one process/catalog per service identity.

To enable hosted artifacts, include the `hosted_artifacts` feature and explicitly
configure all of `artifact_retained_bytes`, `artifact_reserved_bytes`,
`artifact_bytes`, `artifact_upload_bytes`, `artifact_retention_seconds`, and
`relay_bytes`. They default to zero. Service policy intersects these allowances;
a paid plan cannot raise the service's independent safety limits.

Reservation admission is serialized in the artifact service and considers stored
bytes, unfinished reservations and relay headroom for upload plus verification
readback. Duplicate reservations and verified parts reuse existing records.
Each reservation pins its admitted part limit and retention deadline. Downgrades
block new excess reservations while existing uploads may finish within their
reserved bytes, pinned limits and original deadline. Reads/export and deletion
remain available; their measured traffic can exceed the allowance and block new
uploads for the rest of the window. This policy does not promise unlimited relay.

The service records attempted upload bytes, returned download bytes, and logical
storage operations in bounded minute buckets. Failed writes conservatively count
attempted bytes; SDK retries, HTTP overhead and provider-internal operations are
not invoice measurements. The existing maintenance loop reports only aggregate
retained/reserved bytes, current-window relay bytes, operation counts and sample
time. Repeated samples use maxima for window counters, not additive charging.
The Hub never reads the artifact database or receives object keys/content through
this reporting path. Gauges reflect the last received sample and can lag during
an outage; the owning service enforces its allocations locally.

## Cost evidence and retention

Hub telemetry records request counts, response bytes, aggregate handler duration,
heartbeat counts, accepted mutations/events and artifact usage. Telemetry stores
only fixed metric names, integer counts and window timestamps. Expired buckets
are pruned on subsequent samples; no public ingestion or marginal-cost claim is
derived from these counters. Operators must measure idle process/database cost,
heartbeat/request load, database growth, retention and their provider's billed
traffic/operations before setting prices. The metrics are inputs for that review,
not customer model spend or a dollar estimate.

Allocation/grant records, audit records and customer collaboration history are
durable records, distinct from bounded operational telemetry.

## Compatibility and cost drivers

Starting with the capacity catalog retains existing base assignments, grants,
subscription deadlines, stored plan JSON, and Stripe price-to-plan bindings.
Legacy `pilot_free` and `comp_team` versions remain available. Once the capacity
catalog is installed, they resolve with the open-issue guard and without
project, concurrent-work, runner or repository limits. Their stored records and
paid-price bindings stay immutable. Existing pilot organizations keep
complimentary execution access and are labeled as legacy access.
Changing catalogs does not purchase, cancel, or change a subscription. Existing
Stripe price-to-plan mappings are immutable.
Existing custom catalogs must include their historical referenced versions when
adopting new versions. Base changes and scoped grants use the audited entitlement
administrator; Stripe reconciliation continues to own subscription access.

The existing owner billing usage export (`/api/cloud/billing`) adds `cost_drivers`:
active/unarchived and archived native issue counts, separate database and WAL
bytes, current-window request count and request/response bytes, reported artifact
retained/reserved bytes, reported relay bytes, and separately attributable Hub
coordinator AI tokens, turns, and estimated USD cost for the displayed period.
Unmeasured storage/relay inputs, absent request samples, and incompletely priced
AI cost are JSON `null`, meaning unknown. A missing WAL file is measured as zero.
Request bytes are known only when every sampled request declared its body length.
Metrics contain counts and estimates, never customer content or credentials.
These inputs support operator review.

## Subscriptions and AI credits

Hosted billing applies only to the operator's hosted service at `https://cloud.detent.build`; it pays for Detent's hosting, while model-provider accounts, keys and charges stay customer owned. Self-hosted Hubs need no subscription, card or Stripe connection, and choosing an authentication provider never enables billing. Free organizations need neither a card nor a Stripe customer.

The organization billing page shows the effective plan, subscription status, complimentary access, last reconciliation and recent subscription audit entries. Only an organization owner, outside support impersonation, can start Checkout for a plan or open the Stripe customer portal for payment details, invoices, plan changes and cancellation; admins, members, viewers, staff and API or runner tokens cannot. Submitted customer or organization IDs never select the billing account. Returning from Checkout does not change access: the Hub grants a paid plan only after it reconciles the subscription and a paid current invoice with Stripe. Owner-only `GET /api/cloud/billing/subscription` exports the subscription state, entitlement, reconciliation time and recent audit; card numbers, payment methods and webhook payloads are not stored.

Dispatch reads only local entitlements and makes no Stripe request:

| Subscription state | Access |
| --- | --- |
| No subscription | Base plan; no payment record required. |
| Initial incomplete payment | No paid access until Stripe confirms a paid invoice and active subscription. |
| Active with a paid current invoice | The plan through its period end, plus the configured renewal-verification grace. |
| Renewal payment failure or unpaid current invoice | The previously paid plan through its original paid-through deadline plus grace; retries do not restart grace. |
| Grace expired, unpaid, paused or unsupported | Base plan; new allocations must fit the remaining allowances. |
| Verified Stripe trial | The trial plan through the trial end, with no payment grace. |
| Scheduled cancellation | Access ends at Stripe's cancellation time or period end, without grace. |
| Immediate cancellation or downgrade | The new plan at the next authoritative reconciliation. |
| Full refund of the current invoice, or an unresolved or lost dispute | Subscription-derived access is suspended; partial refunds keep it. |

Complimentary grants keep their own scope, reason, expiry and revocation and continue to apply after cancellation, refund, dispute or payment failure. Access reductions block new excess allocations only: running leases still renew and release, work can checkpoint and complete, and reading, export and billing administration stay available. No customer data is deleted.

AI credits are optional prepaid USD balances for Luna chat and other hosted AI usage; no sponsored allowance is included. Owners buy credit packs in billing settings, and verified Stripe events, not the Checkout return, add the credits. Each priced hosted AI turn debits raw provider cost multiplied by the configured `credit_cost_multiplier` (default `1.5`), rounded up to the next microdollar; a $0.10 turn debits $0.15. Usage reports keep the raw provider cost. Purchased credits do not expire. When credits run out, new chat turns stop; in-flight usage is still recorded, including any overrun. Owners can enable auto-fund with a pack and a USD threshold smaller than that pack; it charges the saved payment method only below the threshold, and a decline or authentication failure disables it. Free organizations can use a positive purchased balance.

## Monthly budgets

Monthly budgets cap detected USD spend per organization and per project. Both apply independently: organization exhaustion cannot be overridden by project headroom, and disabling a project policy leaves an enabled organization policy in force. Budgets filter eligibility before capacity acquisition and never change runner grants or issue lanes; `Backlog` work is never promoted by a budget.

`GET` and `PUT` `/api/v2/organizations/{organization}/monthly-budget` and `/api/v2/organizations/{organization}/projects/{project}/monthly-budget` read and update the policies. Organization settings require organization administration; project writes require operator or administrator authority on that project, and project workers can read but not change them. `PUT` takes `idempotency_key`, the current `expected_revision` as a decimal string, and `policy`:

```json
{
  "idempotency_key": "set-monthly-caps-1",
  "expected_revision": "0",
  "policy": {
    "enabled": true,
    "mode": "drain_issue",
    "luna_api_micros": 20000000,
    "runner_api_micros": 100000000,
    "sprite_infrastructure_micros": 50000000,
    "total_micros": 150000000
  }
}
```

Amounts are integer USD millionths; this example sets $20 Luna, $100 runner API and $50 Sprite caps with a $150 total. An omitted cap is unconstrained; an explicit zero closes that bucket. `enabled: false` disables every cap in the policy. `mode` is `drain_issue` (default) or `hard_stop`. Each saved policy version keeps its actor and timestamp.

Spend uses the shared monthly ledger in UTC calendar months, including recorded estimates and late corrections applied to the month of their source interval. The total cap sums the Luna, runner API and Sprite subtotals once. Runner API exposure follows the project's billing mode; local subscription-agent activity has no runner API exposure, and Sprite execution adds infrastructure exposure in either mode. The monthly usage API and the `monthly_usage_costs` MCP tool report unknown costs, unreported and active attempts, estimates and coverage gaps; a known subtotal below a cap is not proof of remaining billing headroom.

With `drain_issue`, exhaustion closes new affected admissions, while already admitted issues continue through validation, review, rework and `Merging`. Sprite exhaustion closes new provisioning and wake work. With `hard_stop`, affected claims and run starts are refused and current leases fail renewal, so the worker cancels its attempt and keeps its issue, evidence and checkpoints; affected Luna turns are cancelled with their messages saved for retry. Recovery is automatic when the month rolls over, a cap is raised or disabled, or a correction lowers the detected subtotal.

These are detected-spend admission controls, not exact provider dollar ceilings. Runner costs usually arrive when attempts finish, infrastructure measurements may be delayed, and both modes have detection and cancellation lag, so parallel work and retries can overrun a cap by an unbounded amount.
