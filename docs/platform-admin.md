# Platform admin console

Status: draft for review. The prototype in `web/conversation/src/app/platform/` implements every page below against fixtures; nothing here is built on the backend yet except what the "Existing" column names.

The platform admin console is what Detent staff see at `/platform` on the shared Cloud entry (hub.detent.build). Today that route is one undesigned list (PR #3127, `PlatformConsole.tsx`). This document specifies the replacement: who uses it, how it is organized, what each page shows and does, and the smallest backend API each page still needs. It reuses the tenant client's design system from #2635 (sidebar layout, grouped settings sections, tables, badges, dialogs) and adds no new visual language.

## Who uses it

Three staff capabilities already exist in the entry's config and code. The console keeps them exactly as they are and never adds a fourth.

| Actor | Config key | Code | What they can do in the console |
|---|---|---|---|
| Platform staff | `staff_emails` | [`platformIdentity`](../internal/cloudentry/platform.go#L22) | Open `/platform`; read every page; take the staff actions below with a reason |
| Entitlement administrator | `entitlement_administrators` (must also be staff) | [`entitlementAdministrator`](../internal/cloudentry/entitlements.go#L92) | Everything staff can, plus see grant details and grant or revoke complimentary plans |
| Support actor | `support_actors` (must also be staff) | [`supportActor`](../internal/cloudentry/service.go#L281) | Everything staff can, plus start and end support access to a ready organization |

Rules the console keeps:

- Platform staff means a staff email signed in with no organization and no impersonation. A support session is not a platform session and cannot open `/platform`.
- Staff never hold tenant membership. The entry refuses organization creation ([provisioning.go#L364](../internal/cloudentry/provisioning.go#L364)) and invitation joins ([login.go#L188](../internal/cloudentry/login.go#L188)) to staff, and every tenant Hub rejects a staff session that is not a support session ([hosted_auth.go#L101](../internal/hubserver/hosted_auth.go#L101)). The console reads tenant data through the entry's own service credential, never by making staff a member.
- Every console read that crosses into a tenant returns metadata only, per the allowlist in [hosted-identity.md](hosted-identity.md) and [cloud-hub-rfc.md](cloud-hub-rfc.md) "Operational reporting": counts, states, emails and roles of members, timestamps. No issue, conversation, repository or artifact content ever reaches the console. Content access is what support access is for, and it is audited in the tenant.
- Every console action that changes something takes a free-text reason, is audited in the entry's `audit` table with the actor's email, and is repeated in the tenant's `hosted_audit` when it touches a tenant.
- The browser decides nothing about access. The client hides controls the session cannot use (it reads `can_support` and `can_grant` from the organizations listing), but the entry re-checks the role on every request, as it does today.

## Information architecture

A persistent left sidebar, the same component and chrome as the tenant app's settings navigation, grouped by the job staff are doing:

```
Detent [Platform]              Platform / Organizations / Harbor Labs        [Proposed API]
                              ---------------------------------------------------------------
Operate                        Harbor Labs                        [Suspend] [Start support access]
  Overview                     ● ready  org_harbor  team
  Organizations                [Overview|Members|Projects|Runners|Usage|Plan|Billing|Provisioning|Audit]
  Users
  Runners & capacity           Registry
  Provisioning & health        ┌──────────────────────────────────────────────────────────────┐
Commercial                     │ Organization id                                   org_harbor │
  Plans & grants               │ Created   By marcus@harborlabs.example            2026-08-28 │
  Billing                      └──────────────────────────────────────────────────────────────┘
Trust
  Support sessions
  Audit log
Configure
  Settings

staff@detent.example
Staff with support and entitlements
Sign out
```

- The header is the tenant app's breadcrumb bar. Detail pages keep their list lit in the sidebar.
- On phones the sidebar becomes the existing sheet, opened from the header.
- Organization detail uses a segmented tab bar (the usage page's `ToggleGroup`), one URL per tab: `/platform/organizations/:id/:tab`.
- Every page shows the same four states: loading (skeleton rows), empty (a sentence that says what will appear and when), failed (the error and "Try again"), and value.

## Pages

Each page lists its purpose, what it shows, its actions, its states, and where the data comes from. "Existing" endpoints are served today; "NEW" ones are proposed in the endpoint table below.

### Overview `/platform`

Purpose: answer "is anything on fire, and is the platform growing" in one screen. The memorable element is the "Needs attention" queue: every item links to the page where staff fix it.

- Shows: counters (organizations, ready, failed, tenant Hubs running, active users over 7 days, runners connected); the attention queue (provisioning failures, tenant Hubs down, failed payments, quarantined Stripe events, capacity near a floor, grants expiring within 14 days), newest critical first; signups in the last 7 days; admission and capacity readings.
- Actions: none on the page; each attention item and signup navigates.
- Empty: "Nothing needs attention" and "No organizations were created this week".
- Data: counters, attention and signups from NEW `GET /overview`; capacity from existing `GET /health`.

### Organizations `/platform/organizations`

- Shows: every registered organization (deleted excluded) with name, creator, state and last error, plan and grant count, members, runners, billing status, created.
- Actions: search by name, id or creator; filter by state and by plan; open an organization.
- Empty: "No organizations are registered yet"; "No organization matches these filters".
- Data: existing `GET /organizations`. Today `plan`, `grants`, `member_count` and `runner_count` are always null and listed in `unavailable`; the page says "Not reported yet" and disables the plan filter until the entry fills them (change 1 below).

### Organization detail `/platform/organizations/:id[/:tab]`

Header: name, state, id, plan, and the actions the organization's state allows.

| Action | Shown when | Who | Effect |
|---|---|---|---|
| Retry provisioning | state `failed` | staff | Resume from the last completed step; the creator's progress page picks it up. Disabled for terminal conflicts that need operator repair. NEW |
| Suspend | state `ready` | staff | Move to `disabled`: revoke sessions, refuse sign-in, runners stop taking work, data and billing untouched. NEW |
| Reactivate | state `disabled` | staff | Back to `ready`. NEW |
| Start support access | state `ready` | support actors | Existing `POST /support/start` form with one of the three reasons; the entry re-checks everything |

Tabs:

- Overview: registry facts (id, WorkOS organization, creator, managed or registered, last failure) and tenant metadata (health, last activity, member/project/runner counts, events, database size). NEW `GET /organizations/:id`, built on the tenant's existing `GET /api/cloud/metadata` ([hosted_ui.go#L243](../internal/hubserver/hosted_ui.go#L243)).
- Members: email, role, status, last seen; pending invitation count. NEW.
- Projects: name, repository and runner counts, last activity. NEW.
- Runners: the fleet table filtered to this organization. NEW.
- Usage: each allowance as used of limit with a meter, over-limit flagged, window reset date. NEW, from the tenant's `HostedEntitlement.usage`.
- Plan: for entitlement administrators, the existing grant and revoke panel unchanged (`ComplimentaryPlans.tsx`, existing `GET/POST .../entitlements`). Other staff see the base plan and grant count only.
- Billing: status, Stripe mode, customer, paid through, grace end, undelivered events. NEW, from the tenant's `hostedBillingReport`.
- Provisioning: the state transition history, newest first, with step, error code and actor. NEW; needs a transitions log (change 3).
- Audit: this organization's entries from the audit log. NEW.

A tab whose tenant does not answer shows the failed state for that tab only.

### Users `/platform/users` and `/platform/users/:subject`

- Shows: everyone who has signed in, with kind (customer, staff, support), organizations and roles, last sign-in. Detail adds organizations created (against the per-identity limit) and each membership.
- Actions: search by email or organization; open a user; jump to a membership's organization.
- Empty: "Nobody has signed in yet".
- Data: NEW `GET /users`, from the entry's `auth.db` sessions joined with the registry's creators and each ready tenant's member list.

### Plans & grants `/platform/plans`

- Shows: the plan catalog from config (each plan's features, allowances, and how many organizations are on it) and the complimentary grants across organizations. Entitlement administrators see each grant's plan, expiry, reason and granter; other staff see only how many organizations have one, matching the administrator check on the existing entitlements endpoint.
- Actions: entitlement administrators get "Manage" on each grant, which opens that organization's Plan tab where the existing grant and revoke dialogs live. No plan editing in v1.
- Data: NEW `GET /plans`. The catalog comes from `allocation.entitlements` config. Grants come from each ready tenant's entitlements (the tenant is authoritative: a grant can apply in the tenant even when the entry's `entitlement_changes` record fails, [entitlements.go#L279](../internal/cloudentry/entitlements.go#L279)), with `granted_by` overlaid from `entitlement_changes` as the existing endpoint does. The response carries `grant_count` for everyone and `grants` only for entitlement administrators.

### Billing `/platform/billing`

- Shows: Stripe mode and account; counters (customers, paying, payment problems, events waiting, quarantined); organizations with payment problems (`payment_failed`, `grace`, `disputed`, `refunded`, `unapproved_plan`); every customer; recent webhook events with delivery status and attempts.
- Actions: "Redeliver" on a quarantined event (reason required). NEW.
- Empty: "Billing is off" when no Stripe account is configured.
- Data: NEW `GET /billing`, from the registry's `billing_customers` and `billing_events` and each tenant's billing status.

### Runners & capacity `/platform/runners`

- Shows: counters (runners, healthy, stale or paused, slots in use, behind the Hub's version, Hub version); organizations that did not answer, named, so a partial list is never mistaken for a full one; every runner with organization, health, slots, version, last heartbeat.
- Actions: none in v1; each runner links to its organization.
- Data: NEW `GET /runners`, fanned out to each ready tenant's fleet with the entry's service credential.

### Provisioning & health `/platform/provisioning`

- Shows: the entry's admission readings (registry, tenant Hubs running, tenant slots with a meter, provisions in flight, free disk and available memory against their floors) and one row per tenant Hub: running or down, up since, restarts, database size, last backup. When no backup schedule exists the page says so plainly: backups are manual (`detent hub backup` on a stopped Hub) and no restore has been rehearsed.
- Data: existing `GET /health`; NEW `GET /tenants`, which needs the launcher to keep its process facts (change 4).

### Audit log `/platform/audit`

- Shows: entries newest first with when, actor and actor kind (staff, support, member, system), event, organization, detail.
- Actions: search; filter by actor kind; older entries by cursor.
- Data: NEW `GET /audit`, from the entry's `audit` table joined with session emails; tenant `hosted_audit` rows appear on the organization's Audit tab only.

### Support sessions `/platform/support`

- Shows: sessions active now (actor, organization, signed in as, reason, time left) and past sessions with outcome (ended, expired).
- Actions: support actors can "End session" (reason required), which revokes the authorization at once. Starting a session stays on the organization page. NEW.
- Data: NEW `GET /support-sessions`, from `auth.db` `authorizations` where `support = 1` and `transactions`.

### Settings `/platform/settings`

- Shows, read-only: signup policy (self-service on or off, open or allowlist, allowed emails and domains, source file and keys); people (staff, support actors, entitlement administrators); allocation limits; billing mode and account.
- Actions: none. The page says the file is the source of truth and a restart applies changes.
- Data: existing `GET /allowlist`; NEW `GET /settings` (a redacted projection of the loaded config; no secrets, token env names or webhook secrets).

## Endpoints

All paths are under `/api/cloud/platform` on the entry, JSON, same session and CSRF rules as today. Reads require platform staff and audit a `platform_*_viewed` event like the existing ones. Writes require CSRF, a reason, and the role named.

| Page | Method and path | Status | Source |
|---|---|---|---|
| All (access) | `GET /organizations` | Existing | [platform.go#L144](../internal/cloudentry/platform.go#L144) |
| Organizations | `GET /organizations` fills `plan`, `grants`, `member_count`, `runner_count` | Change 1 | fan out to tenant `/api/cloud/metadata` and `/plan` via [`eachReadyTenant`](../internal/cloudentry/platform.go#L104) |
| Settings | `GET /allowlist` | Existing | [platform.go#L171](../internal/cloudentry/platform.go#L171) |
| Overview, Provisioning | `GET /health` | Existing | [platform.go#L204](../internal/cloudentry/platform.go#L204) |
| Organization / Plan | `GET, POST /organizations/:id/entitlements` | Existing (entitlement admins) | [entitlements.go#L188](../internal/cloudentry/entitlements.go#L188), [#L217](../internal/cloudentry/entitlements.go#L217) |
| Organization | `POST /support/start` (form, outside this prefix) | Existing (support actors) | [support.go#L40](../internal/cloudentry/support.go#L40) |
| Overview | `GET /overview` | NEW | registry counts, `sessions` for active users, attention rules over registry, `billing_events`, health, grants |
| Organization | `GET /organizations/:id` | NEW | registry row + tenant `/api/cloud/metadata` |
| Organization | `GET /organizations/:id/members` | NEW | tenant members, metadata fields only |
| Organization | `GET /organizations/:id/projects` | NEW | tenant projects, counts only |
| Organization | `GET /organizations/:id/runners` | NEW | tenant fleet |
| Organization | `GET /organizations/:id/usage` | NEW | tenant `HostedEntitlement` allowances and usage |
| Organization | `GET /organizations/:id/billing` | NEW | tenant `hostedBillingReport` |
| Organization | `GET /organizations/:id/provisioning` | NEW | transitions log (change 3) |
| Organization | `POST /organizations/:id/provisioning/retry` | NEW (staff) | clears `error_code`, schedules the allocator |
| Organization | `POST /organizations/:id/suspend`, `/reactivate` | NEW (staff) | `ready`↔`disabled` (change 2) |
| Users | `GET /users` | NEW | `auth.db` sessions + registry creators + tenant members |
| Runners | `GET /runners` | NEW | fan-out of tenant fleets, with `unreachable` |
| Provisioning | `GET /tenants` | NEW | launcher process facts (change 4) + tenant metadata |
| Plans | `GET /plans` | NEW | config catalog + tenant entitlements fan-out, `granted_by` from `entitlement_changes`; grant details for entitlement admins only |
| Billing | `GET /billing` | NEW | `billing_customers`, `billing_events`, tenant statuses |
| Billing | `POST /billing/events/:id/redeliver` | NEW (staff) | requeue a quarantined `billing_events` row |
| Audit | `GET /audit?q=&actor_kind=&organization=&cursor=` | NEW | entry `audit` + `sessions` emails |
| Support | `GET /support-sessions` | NEW | `authorizations` (support) + `transactions` |
| Support | `POST /support-sessions/:id/end` | NEW (support actors) | revoke the authorization |
| Settings | `GET /settings` | NEW | redacted loaded config |

The response shapes are the schemas in [`web/conversation/src/contracts/platform.ts`](../web/conversation/src/contracts/platform.ts), with realistic examples in `src/contracts/fixtures/platform/`. When an endpoint is built, its fixture moves to the shared fixture directory the Go contract test reads.

Backend changes the NEW endpoints depend on:

1. Fill the organizations listing's four null fields with one fan-out per request, bounded by the existing 8 workers and 8 s deadline.
2. A `ready`↔`disabled` transition the entry can make, plus revoking the organization's sessions. Today only `Register` sets `disabled`; sign-in already refuses any organization that is not `ready` ([login.go#L44](../internal/cloudentry/login.go#L44)).
3. A provisioning transitions log. Today only the latest failure is kept on the organization row and `organization_events` records registrations only.
4. Launcher process facts (started at, restarts) kept in memory and exposed, and a backup record per tenant if backups become scheduled.
5. Emails in the entry audit view. The `audit` table stores subjects only; join `sessions` at read time.

Mechanisms: changes 2 and 3 add a state transition and a log, not a new lease, brake or recovery loop. Retry provisioning reuses the allocator's existing retry path.

## Prototype

- Routes: `web/conversation/src/app/platform/`, mounted under the entry router's `/platform`. The entry now serves the client shell for `/platform/*` as well as `/platform` (same staff check and `platform_opened` audit), so a detail URL loads directly. The old `PlatformConsole.tsx` stays until the real build replaces it.
- Data: `src/app/platform/api.ts` separates `live` endpoints (served today) from proposed ones. Outside preview mode the client never requests a proposed endpoint; the page shows "Not available yet" naming the endpoint. Preview mode is on only in a dev server started by `npm run dev:mock` (the check requires `import.meta.env.DEV`, so no build can turn it on), and fixtures live only in the mock hub (`dev/mock-platform.ts`), so no fixture data can reach the production bundle.
- Run it: `cd web/conversation && npm run dev:mock`, then open `/platform`. Switch states with `/__mock/platform?scenario=full|empty|error&role=admin|staff|forbidden|signed-out`.
- Tests: `tests/components/platformAdmin.test.tsx` (routing, role gating, live/proposed separation, filters) and `src/contracts/platform.test.ts` (every fixture decodes).
- Screenshots: `tests/visual/platform-admin/`, produced by `tests/visual/platform-admin.spec.js` against a running mock.

## Out of scope for v1

- Editing configuration from the console: staff lists, support actors, entitlement administrators, allowlist, allocation limits, Stripe mode. The config file stays the source of truth.
- Editing the plan catalog or prices; only complimentary grants change entitlements.
- Deleting organizations as staff, impersonating without support access, or any cross-tenant content view.
- Refunds, invoices, coupons or anything else that belongs in the Stripe dashboard.
- Runner actions (pause, drain, upgrade) and tenant process actions (restart, stop).
- Backup scheduling and restore from the console.
- Charts and trends over time; v1 is current state plus lists.
- Notifications or alert routing outside the console.
