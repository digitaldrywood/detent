// Wire contracts for the platform console's proposed endpoints.
//
// The entry serves four platform endpoints today; their schemas live with the
// entry client in `src/app/entry/api.ts`. Everything here is the smallest API
// `docs/platform-admin.md` proposes for the rest of the console. None of it is
// served yet: the fixtures under `fixtures/platform/` are what the mock hub
// answers with, and they are deliberately outside the cross-language fixture
// directory the Go contract test reads.
import * as Schema from "effect/Schema";

const Timestamp = Schema.String;
const OptionalTimestamp = Schema.NullOr(Schema.String);

export const OrganizationState = Schema.Literals([
  "requested",
  "allocating",
  "ready",
  "failed",
  "disabled",
  "deleting",
  "deleted",
]);
export type OrganizationState = typeof OrganizationState.Type;

export const Severity = Schema.Literals(["critical", "warning", "info"]);
export type Severity = typeof Severity.Type;

export const OrganizationRef = Schema.Struct({ id: Schema.String, name: Schema.String });
export type OrganizationRef = typeof OrganizationRef.Type;

// --- Overview ---------------------------------------------------------------

export const AttentionItem = Schema.Struct({
  id: Schema.String,
  kind: Schema.Literals([
    "provisioning_failed",
    "tenant_down",
    "payment_failed",
    "billing_quarantined",
    "capacity_low",
    "grant_expiring",
  ]),
  severity: Severity,
  title: Schema.String,
  detail: Schema.String,
  organization: Schema.NullOr(OrganizationRef),
  since: Timestamp,
});
export type AttentionItem = typeof AttentionItem.Type;

export const Signup = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  creator_email: Schema.String,
  state: OrganizationState,
  created_at: Timestamp,
});
export type Signup = typeof Signup.Type;

export const PlatformOverview = Schema.Struct({
  generated_at: Timestamp,
  organizations: Schema.Struct({
    total: Schema.Number,
    by_state: Schema.Record(Schema.String, Schema.Number),
  }),
  users: Schema.Struct({ total: Schema.Number, active_7d: Schema.Number }),
  runners: Schema.Struct({ connected: Schema.Number, registered: Schema.Number }),
  signups: Schema.Struct({ last_7d: Schema.Number, recent: Schema.Array(Signup) }),
  attention: Schema.Array(AttentionItem),
});
export type PlatformOverview = typeof PlatformOverview.Type;

// --- Organization detail ----------------------------------------------------

export const TenantMetadata = Schema.Struct({
  members: Schema.Number,
  projects: Schema.Number,
  runners: Schema.Number,
  events: Schema.Number,
  last_activity: OptionalTimestamp,
  database_bytes: Schema.Number,
  healthy: Schema.Boolean,
});
export type TenantMetadata = typeof TenantMetadata.Type;

export const OrganizationDetail = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  state: OrganizationState,
  step: Schema.String,
  attempts: Schema.Number,
  error_code: Schema.String,
  managed: Schema.Boolean,
  provider_id: Schema.String,
  creator_email: Schema.String,
  created_at: Timestamp,
  updated_at: Timestamp,
  plan: Schema.NullOr(Schema.String),
  billing_status: Schema.NullOr(Schema.String),
  metadata: Schema.NullOr(TenantMetadata),
  can_retry: Schema.Boolean,
  can_suspend: Schema.Boolean,
  can_reactivate: Schema.Boolean,
});
export type OrganizationDetail = typeof OrganizationDetail.Type;

export const OrganizationMember = Schema.Struct({
  id: Schema.String,
  email: Schema.String,
  role: Schema.Literals(["owner", "admin", "member", "viewer"]),
  status: Schema.String,
  last_seen_at: OptionalTimestamp,
});
export type OrganizationMember = typeof OrganizationMember.Type;

export const OrganizationMembers = Schema.Struct({
  members: Schema.Array(OrganizationMember),
  pending_invitations: Schema.Number,
});
export type OrganizationMembers = typeof OrganizationMembers.Type;

export const OrganizationProject = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  repositories: Schema.Number,
  runners: Schema.Number,
  last_activity: OptionalTimestamp,
});
export type OrganizationProject = typeof OrganizationProject.Type;

export const OrganizationProjects = Schema.Struct({ projects: Schema.Array(OrganizationProject) });
export type OrganizationProjects = typeof OrganizationProjects.Type;

export const Allowance = Schema.Struct({ name: Schema.String, used: Schema.Number, limit: Schema.Number });
export type Allowance = typeof Allowance.Type;

export const OrganizationUsage = Schema.Struct({
  window_ends_at: OptionalTimestamp,
  allowances: Schema.Array(Allowance),
});
export type OrganizationUsage = typeof OrganizationUsage.Type;

export const OrganizationBilling = Schema.Struct({
  enabled: Schema.Boolean,
  mode: Schema.NullOr(Schema.Literals(["test", "live"])),
  customer_id: Schema.NullOr(Schema.String),
  status: Schema.String,
  paid_through: OptionalTimestamp,
  access_until: OptionalTimestamp,
  grace_until: OptionalTimestamp,
  pending_events: Schema.Number,
});
export type OrganizationBilling = typeof OrganizationBilling.Type;

export const ProvisioningEvent = Schema.Struct({
  at: Timestamp,
  from: Schema.NullOr(OrganizationState),
  to: OrganizationState,
  step: Schema.String,
  error_code: Schema.String,
  actor: Schema.String,
});
export type ProvisioningEvent = typeof ProvisioningEvent.Type;

export const ProvisioningHistory = Schema.Struct({ events: Schema.Array(ProvisioningEvent) });
export type ProvisioningHistory = typeof ProvisioningHistory.Type;

// --- Runners ----------------------------------------------------------------

export const PlatformRunner = Schema.Struct({
  id: Schema.String,
  organization: OrganizationRef,
  display_name: Schema.String,
  hostname: Schema.String,
  health: Schema.String,
  state: Schema.String,
  os: Schema.String,
  architecture: Schema.String,
  version: Schema.String,
  host_capacity: Schema.Number,
  host_used: Schema.Number,
  last_heartbeat_at: OptionalTimestamp,
});
export type PlatformRunner = typeof PlatformRunner.Type;

export const PlatformRunners = Schema.Struct({
  current_version: Schema.String,
  runners: Schema.Array(PlatformRunner),
  unreachable: Schema.Array(OrganizationRef),
});
export type PlatformRunners = typeof PlatformRunners.Type;

// --- Users ------------------------------------------------------------------

export const UserMembership = Schema.Struct({
  organization: OrganizationRef,
  role: Schema.String,
  status: Schema.String,
});
export type UserMembership = typeof UserMembership.Type;

export const PlatformUser = Schema.Struct({
  subject: Schema.String,
  email: Schema.String,
  kind: Schema.Literals(["customer", "staff", "support"]),
  last_sign_in_at: OptionalTimestamp,
  organizations: Schema.Array(UserMembership),
  created_organizations: Schema.Number,
});
export type PlatformUser = typeof PlatformUser.Type;

export const PlatformUsers = Schema.Struct({ users: Schema.Array(PlatformUser) });
export type PlatformUsers = typeof PlatformUsers.Type;

// --- Plans ------------------------------------------------------------------

export const CatalogPlan = Schema.Struct({
  id: Schema.String,
  version: Schema.Number,
  features: Schema.Array(Schema.String),
  allowances: Schema.Record(Schema.String, Schema.Number),
  organizations: Schema.Number,
  default: Schema.Boolean,
});
export type CatalogPlan = typeof CatalogPlan.Type;

export const PlatformGrant = Schema.Struct({
  id: Schema.String,
  organization: OrganizationRef,
  plan: Schema.Struct({ id: Schema.String, version: Schema.Number }),
  starts_at: Timestamp,
  expires_at: OptionalTimestamp,
  reason: Schema.String,
  granted_by: Schema.String,
});
export type PlatformGrant = typeof PlatformGrant.Type;

/** `grants` is filled only for entitlement administrators; everyone else gets the count. */
export const PlatformPlans = Schema.Struct({
  plans: Schema.Array(CatalogPlan),
  grant_count: Schema.Number,
  grants: Schema.Array(PlatformGrant),
});
export type PlatformPlans = typeof PlatformPlans.Type;

// --- Billing ----------------------------------------------------------------

export const BillingCustomer = Schema.Struct({
  organization: OrganizationRef,
  customer_id: Schema.String,
  status: Schema.String,
  plan: Schema.NullOr(Schema.String),
  paid_through: OptionalTimestamp,
});
export type BillingCustomer = typeof BillingCustomer.Type;

export const BillingEvent = Schema.Struct({
  event_id: Schema.String,
  event_type: Schema.String,
  organization: Schema.NullOr(OrganizationRef),
  status: Schema.Literals(["pending", "delivered", "ignored", "quarantined"]),
  attempts: Schema.Number,
  received_at: Timestamp,
});
export type BillingEvent = typeof BillingEvent.Type;

export const PlatformBilling = Schema.Struct({
  mode: Schema.NullOr(Schema.Literals(["test", "live"])),
  account_id: Schema.NullOr(Schema.String),
  customers: Schema.Array(BillingCustomer),
  events: Schema.Array(BillingEvent),
});
export type PlatformBilling = typeof PlatformBilling.Type;

// --- Provisioning and tenant health -----------------------------------------

export const TenantProcess = Schema.Struct({
  organization: OrganizationRef,
  state: OrganizationState,
  running: Schema.Boolean,
  started_at: OptionalTimestamp,
  restarts: Schema.Number,
  database_bytes: Schema.Number,
  last_backup_at: OptionalTimestamp,
});
export type TenantProcess = typeof TenantProcess.Type;

export const PlatformTenants = Schema.Struct({
  backups_configured: Schema.Boolean,
  tenants: Schema.Array(TenantProcess),
});
export type PlatformTenants = typeof PlatformTenants.Type;

// --- Audit and support ------------------------------------------------------

export const AuditEntry = Schema.Struct({
  id: Schema.String,
  at: Timestamp,
  actor: Schema.String,
  actor_kind: Schema.Literals(["staff", "support", "member", "system"]),
  event: Schema.String,
  organization: Schema.NullOr(OrganizationRef),
  detail: Schema.String,
});
export type AuditEntry = typeof AuditEntry.Type;

export const AuditPage = Schema.Struct({
  entries: Schema.Array(AuditEntry),
  next_cursor: Schema.NullOr(Schema.String),
});
export type AuditPage = typeof AuditPage.Type;

export const SupportSession = Schema.Struct({
  id: Schema.String,
  actor: Schema.String,
  organization: OrganizationRef,
  effective_email: Schema.String,
  reason: Schema.String,
  state: Schema.Literals(["pending", "active", "ended", "expired"]),
  started_at: Timestamp,
  expires_at: Timestamp,
  ended_at: OptionalTimestamp,
});
export type SupportSession = typeof SupportSession.Type;

export const SupportSessions = Schema.Struct({ sessions: Schema.Array(SupportSession) });
export type SupportSessions = typeof SupportSessions.Type;

// --- Settings ---------------------------------------------------------------

export const PlatformSettings = Schema.Struct({
  source: Schema.Struct({ file: Schema.String }),
  staff_emails: Schema.Array(Schema.String),
  support_actors: Schema.Array(Schema.String),
  entitlement_administrators: Schema.Array(Schema.String),
  allocation: Schema.Struct({
    max_tenants: Schema.Number,
    max_concurrent_provisions: Schema.Number,
    max_organizations_per_identity: Schema.Number,
    retry_limit: Schema.Number,
    min_free_disk_bytes: Schema.Number,
    min_available_memory_bytes: Schema.Number,
  }),
  billing: Schema.Struct({
    mode: Schema.NullOr(Schema.Literals(["test", "live"])),
    account_id: Schema.NullOr(Schema.String),
  }),
});
export type PlatformSettings = typeof PlatformSettings.Type;

export const ActionResult = Schema.Struct({ ok: Schema.Boolean, message: Schema.String });
export type ActionResult = typeof ActionResult.Type;

/** One organization's proposed reads together, as the mock hub stores them. */
export const OrganizationRecord = Schema.Struct({
  detail: OrganizationDetail,
  members: OrganizationMembers,
  projects: OrganizationProjects,
  usage: OrganizationUsage,
  billing: OrganizationBilling,
  provisioning: ProvisioningHistory,
});
export type OrganizationRecord = typeof OrganizationRecord.Type;

/** The instant the platform fixtures were written against; the mock shifts them to now. */
export const PLATFORM_FIXTURE_NOW = "2026-09-27T18:00:00Z";
