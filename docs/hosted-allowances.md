# Hosted capacity allowances

Hosted identity enables organization entitlements in the owning Hub database.
Local and self-hosted Hubs do not create assignments, query a billing service, or
apply hosted restrictions. No Stripe account, subscription, or card is necessary
for the permanent Free base plan. Model-provider charges remain separate from hosting.

## Capacity catalog

| Plan | USD per organization/month | Projects | Unarchived issues |
| --- | ---: | ---: | ---: |
| Free | 0 | 1 | 200 |
| Starter | 49 | 5 | 2,000 |
| Growth | 149 | 25 | 10,000 |
| Scale | 399 | 100 | 50,000 |

Omitted or empty `entitlements` selects this catalog with immutable version-1
IDs `free`, `starter`, `growth`, and `scale`. Enterprise is custom and has no
self-serve price. Free signup needs no card, Stripe customer, or subscription.
Free permits project/issue exploration and refuses AI execution, including
ordinary Luna coordinator chat and issue-linked runner claims. Paid hosting
contains no bundled AI-dollar allowance; customer-provider funding still applies.
An explicitly scoped execution grant can authorize complimentary execution.

Quotas count stored projects and unarchived native issues across the organization.
Issue creation, imports, and restore use the existing atomic mutation transaction.
At or above a downgraded quota, existing data, reading, export, billing, archiving,
and safe in-flight completion remain available. Archiving frees capacity without
deleting history. New allocations and restore must fit the current capacity.
Membership and concurrent-work quantities are measured but never tier admission
limits, including on legacy plans. Runner, host, policy, and provider safety limits
continue to apply independently. Other service-protection limits are common to
the approved tiers rather than paid concurrency or seat entitlements.

## Configuration and plan versions

An explicit catalog replaces the default and must retain any referenced legacy
versions. The following is the legacy pilot catalog, retained for compatibility;
use the default capacity catalog for new deployments:

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

Omitting the entire section, or leaving it empty (`entitlements: {}`), selects
exactly this catalog and these pilot settings. Any key inside the section, even
`plans: []`, makes it an explicit catalog that must validate:
every organization starts on `pilot_free`, and `comp_team` exists only to be
granted as complimentary access. `comp_team` doubles the `pilot_free` allowances
and adds `hosted_artifacts`; its artifact allowances stay zero until an operator
configures them (see below). Both sets of numbers are provisional.
Within an explicit plan, omitted allowances are zero and omitted features are
disabled. Negative values, unknown names and unsupported configuration bounds
are rejected. Windows are whole multiples of 60 seconds, at most one day;
telemetry retains at most 720 windows and 30 days. Invitation reservations last
between one minute and seven days. These are validation bounds, not recommended
public retention periods. Operators choose the values before hosting customers.

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
generated secret material. Do not reuse the metadata reporter token. Ordinary
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
and effective plan, current revision, configured plans, and every grant with its
recorded reason and administrator identity. No other credential can read it.

Grant `starts_at` defaults to transaction time; optional `expires_at` uses an
RFC3339 timestamp after the start. A `revoke` command names `grant_id` and a
reason. `base` assigns a plan version. `subscription` assigns a plan version
with a required future validity deadline; `end_subscription` clears that
derived access when Stripe billing is disabled. With
[hosted subscription billing](hosted-billing.md) configured, authoritative
Stripe reconciliation owns subscription-derived access and these two commands
are rejected. Complimentary grants remain independent of payment records.

### Granting from the platform console

On the shared entry, registry platform members with `admin` or `billing` roles
may grant and revoke complimentary plans. Configure
`allocation.entitlement_admin_token_env` so the entry can reach tenant entitlements.
`platform.bootstrap_admin_email` restores the operator as an admin at startup.
The deprecated `entitlement_administrators`, `support_actors`, and `staff_emails`
seed an empty platform membership registry; they do not gate access after seeding.
See the [shared entry example](examples/hub/README.md#shared-entry).

Authorized members can read the base plan, effective plan and active grants, and
submit grant or revoke requests with a required reason. `support` and `viewer`
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
| Concurrent work | Unreleased leases whose expiry is strictly after the transaction clock. No plan concurrency limit applies. Operational runner and host capacity still apply. |
| API mutations | Accepted allocation-changing transactions per configured UTC window. Matching native command retries do not consume another unit. Reads, billing, renew/release and completion/checkpoint events remain available. |
| Ingested events | Newly stored collaboration events per window. Native retries and ordered duplicate event sequences do not add an event. |
| Collaboration bytes | UTF-8 bytes of issue text/labels/assignees, comments, versions, event payload/actor records, idempotency responses, attempts, artifact references and import snapshots. This is logical retained payload, not disk billing. Database/WAL bytes are separately reported. |
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
durable records, distinct from bounded operational telemetry. This change does
not silently purge customer history, alter public retention promises, configure
WorkOS, enable a production artifact service or launch paid billing.

## Compatibility and cost drivers

Starting with the capacity catalog retains existing base assignments, grants,
subscription deadlines, stored plan JSON, and Stripe price-to-plan bindings.
Legacy `pilot_free` and `comp_team` versions remain available. Once the capacity
catalog is installed, their resolved capacity includes at least Starter's 2,000
unarchived issues and retains any larger legacy project quota. Their stored
records and paid-price bindings stay immutable; plan runner/repository count
ceilings are removed in this compatibility view. Existing pilot organizations
keep complimentary execution access and are labeled as legacy access.
Changing catalogs does not purchase, cancel, or change a subscription. Never
repoint the existing $49 test price: retain its exact versioned-plan mapping.
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
These inputs support operator review; #2308 owns operating-cost measurements.
