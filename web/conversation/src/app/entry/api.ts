// The shared entry's JSON surface: the organization chooser, self-service
// creation and its provisioning status. These live on
// the shared origin itself, outside any organization's base path, and use the
// entry session's CSRF token from the chooser payload.
import * as Schema from "effect/Schema";

import { AccountError, type FetchLike } from "../account/api.ts";

export const EntryOrganization = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  url: Schema.String,
  state: Schema.optional(Schema.String),
  role: Schema.optional(Schema.String),
});
export type EntryOrganization = typeof EntryOrganization.Type;

export const EntryOrganizations = Schema.Struct({
  email: Schema.String,
  csrf: Schema.String,
  organizations: Schema.Array(EntryOrganization),
  pending: Schema.optional(Schema.Array(EntryOrganization)),
  can_create: Schema.optional(Schema.Boolean),
  platform_role: Schema.optional(Schema.String),
});
export type EntryOrganizations = typeof EntryOrganizations.Type;

export const EntrySession = Schema.Struct({
  email: Schema.String,
  csrf: Schema.String,
  can_create: Schema.optional(Schema.Boolean),
  platform_role: Schema.optional(Schema.String),
});
export type EntrySession = typeof EntrySession.Type;

export const Provisioning = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  state: Schema.String,
  step: Schema.String,
  error: Schema.String,
  can_resume: Schema.Boolean,
  next: Schema.optional(Schema.String),
});
export type Provisioning = typeof Provisioning.Type;

export const NextResult = Schema.Struct({ next: Schema.String });
export type NextResult = typeof NextResult.Type;

export const PlatformOrganization = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  state: Schema.String,
  step: Schema.String,
  attempts: Schema.Number,
  error_code: Schema.String,
  error_detail: Schema.optional(Schema.String),
  managed: Schema.Boolean,
  creator_email: Schema.String,
  created_at: Schema.String,
  updated_at: Schema.String,
  billing: Schema.Struct({ available: Schema.Boolean, status: Schema.optional(Schema.String), customer_id: Schema.optional(Schema.String), plan: Schema.optional(Schema.String), price_label: Schema.optional(Schema.String) }),
  can_support: Schema.Boolean,
  plan: Schema.NullOr(Schema.String),
  grants: Schema.NullOr(Schema.Number),
  member_count: Schema.NullOr(Schema.Number),
  runner_count: Schema.NullOr(Schema.Number),
});
export type PlatformOrganization = typeof PlatformOrganization.Type;

export const PlatformOrganizations = Schema.Struct({
  email: Schema.String,
  csrf: Schema.String,
  can_support: Schema.Boolean,
  can_grant: Schema.optional(Schema.Boolean),
  organizations: Schema.Array(PlatformOrganization),
  unavailable: Schema.Array(Schema.String),
});
export type PlatformOrganizations = typeof PlatformOrganizations.Type;

export const PlatformAuditRow = Schema.Struct({
  at: Schema.String,
  actor: Schema.String,
  event: Schema.String,
  organization_id: Schema.String,
  organization_name: Schema.String,
  organization_deleted: Schema.Boolean,
  detail: Schema.String,
  source: Schema.String,
  id: Schema.String,
});
export type PlatformAuditRow = typeof PlatformAuditRow.Type;

export const PlatformAudit = Schema.Struct({
  rows: Schema.Array(PlatformAuditRow),
  events: Schema.Array(Schema.String),
  tenants: Schema.Array(Schema.Struct({ id: Schema.String, name: Schema.String, deleted: Schema.Boolean })),
  next_cursor: Schema.String,
});
export type PlatformAudit = typeof PlatformAudit.Type;

const AccountOrganization = { organization_id: Schema.String, organization_name: Schema.String };
export const PlatformAccounts = Schema.Struct({
  accounts: Schema.Array(Schema.Struct({
    email: Schema.String,
    subject: Schema.String,
    platform_role: Schema.String,
    last_sign_in_at: Schema.String,
    memberships: Schema.Array(Schema.Struct({ ...AccountOrganization, role: Schema.String, joined_at: Schema.String })),
    invitations: Schema.Array(Schema.Struct({ ...AccountOrganization, expires_at: Schema.String })),
  })),
  unsearched: Schema.Array(Schema.Struct(AccountOrganization)),
});
export type PlatformAccounts = typeof PlatformAccounts.Type;

export const PlanReference = Schema.Struct({ id: Schema.String, version: Schema.Number });
export type PlanReference = typeof PlanReference.Type;

export const EntitlementPlan = Schema.Struct({
  id: Schema.String,
  version: Schema.Number,
  features: Schema.NullOr(Schema.Array(Schema.String)),
  allowances: Schema.NullOr(Schema.Record(Schema.String, Schema.Number)),
});
export type EntitlementPlan = typeof EntitlementPlan.Type;

export const EntitlementGrant = Schema.Struct({
  id: Schema.String,
  plan: PlanReference,
  scope: Schema.NullOr(Schema.Array(Schema.String)),
  starts_at: Schema.String,
  expires_at: Schema.NullOr(Schema.String),
  reason: Schema.String,
  granted_by: Schema.String,
  granted_at: Schema.NullOr(Schema.String),
});
export type EntitlementGrant = typeof EntitlementGrant.Type;

export const OrganizationEntitlements = Schema.Struct({
  organization_id: Schema.String,
  base: PlanReference,
  effective_base: PlanReference,
  source: Schema.String,
  revision: Schema.Number,
  features: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
  grants: Schema.Array(EntitlementGrant),
  plans: Schema.Array(EntitlementPlan),
});
export type OrganizationEntitlements = typeof OrganizationEntitlements.Type;

export const PlatformTenantDetail = Schema.Struct({
  organization: PlatformOrganization,
  csrf: Schema.String,
  can_grant: Schema.Boolean,
  can_resume: Schema.Boolean,
  unavailable: Schema.Array(Schema.String),
  members: Schema.NullOr(Schema.Array(Schema.Struct({ id: Schema.String, email: Schema.String, role: Schema.String }))),
  runners: Schema.NullOr(Schema.Array(Schema.Struct({ id: Schema.String, name: Schema.String, health: Schema.String }))),
  projects: Schema.NullOr(Schema.Array(Schema.Struct({ id: Schema.String, name: Schema.String }))),
  entitlements: Schema.NullOr(OrganizationEntitlements),
  events: Schema.Array(Schema.Struct({ event: Schema.String, generation: Schema.Number, recorded_at: Schema.String })),
});
export type PlatformTenantDetail = typeof PlatformTenantDetail.Type;

export type EntitlementChange =
  | {
      readonly action: "grant";
      readonly feature: "model_choice";
      readonly idempotency_key: string;
      readonly expected_revision: number;
      readonly expires_at: string | null;
      readonly reason: string;
    }
  | {
      readonly action: "grant";
      readonly idempotency_key: string;
      readonly expected_revision: number;
      readonly plan: PlanReference;
      readonly expires_at: string | null;
      readonly reason: string;
    }
  | {
      readonly action: "revoke";
      readonly idempotency_key: string;
      readonly expected_revision: number;
      readonly grant_id: string;
      readonly reason: string;
    };

export const EntitlementChangeResult = Schema.Struct({ action: Schema.String, grant_id: Schema.String });
export type EntitlementChangeResult = typeof EntitlementChangeResult.Type;

export const PlatformMember = Schema.Struct({
  email: Schema.String,
  role: Schema.String,
  added_by: Schema.String,
  added_at: Schema.String,
  updated_at: Schema.String,
  bootstrap: Schema.Boolean,
});
export type PlatformMember = typeof PlatformMember.Type;

export const PlatformMembers = Schema.Struct({
  members: Schema.Array(PlatformMember),
  revision: Schema.Number,
  self: Schema.Struct({ email: Schema.String, role: Schema.String }),
});
export type PlatformMembers = typeof PlatformMembers.Type;

export type PlatformMemberChange = {
  readonly action: "add" | "change" | "remove";
  readonly email: string;
  readonly role?: string;
  readonly reason: string;
  readonly idempotency_key: string;
  readonly expected_revision: number;
};

export const PlatformMemberChangeResult = Schema.Struct({ email: Schema.String, role: Schema.String, revision: Schema.Number });

export const PlatformAllowlist = Schema.Struct({
  self_service: Schema.Boolean,
  open: Schema.optional(Schema.Boolean),
  allowed_emails: Schema.Array(Schema.String),
  allowed_domains: Schema.Array(Schema.String),
  source: Schema.Struct({ file: Schema.String, keys: Schema.Array(Schema.String) }),
});
export type PlatformAllowlist = typeof PlatformAllowlist.Type;

export const PlatformHealth = Schema.Struct({
  registry: Schema.Struct({ ok: Schema.Boolean }),
  tenants: Schema.optional(Schema.Struct({ expected: Schema.Number, running: Schema.Number })),
  admission: Schema.optional(
    Schema.Struct({
      tenants: Schema.Number,
      max_tenants: Schema.Number,
      allocating: Schema.Number,
      max_concurrent: Schema.Number,
      disk_measured: Schema.Boolean,
      free_disk_bytes: Schema.Number,
      min_free_disk_bytes: Schema.Number,
      memory_measured: Schema.Boolean,
      available_memory_bytes: Schema.Number,
      min_available_memory_bytes: Schema.Number,
    }),
  ),
});
export type PlatformHealth = typeof PlatformHealth.Type;

export const PLATFORM = "/platform/tenants";
export const SUPPORT_REASONS = ["customer-request", "account-recovery", "troubleshooting"] as const;

export const CHOOSE_ORGANIZATION = "/organizations?switch=1";
export const SIGN_IN_ORGANIZATIONS = "/auth/oidc/start?return=%2Forganizations";
export const SIGN_IN_PLATFORM = "/auth/oidc/start?return=%2Fplatform";

function statusMessage(status: number): string {
  if (status === 401) return "Your session has expired. Sign in again.";
  if (status === 403) return "You do not have permission to do that.";
  if (status === 404) return "That organization is not available to this account.";
  if (status === 429) return "Your account has reached its organization limit.";
  if (status >= 500) return "Detent could not complete the request. Try again shortly.";
  return `The request failed (${status}).`;
}

async function decodeFailure(response: Response): Promise<AccountError> {
  let body: unknown = null;
  try {
    body = await response.json();
  } catch {
    body = null;
  }
  const record = body as { code?: unknown; message?: unknown } | null;
  return new AccountError({
    status: response.status,
    code: typeof record?.code === "string" ? record.code : "request_failed",
    message: typeof record?.message === "string" ? record.message : statusMessage(response.status),
  });
}

export function makeEntryApi(options: { readonly fetch?: FetchLike; readonly origin?: string } = {}) {
  const fetchImpl: FetchLike = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  const origin = options.origin ?? "";

  async function read<A>(schema: Schema.Codec<A, any, never, never>, path: string): Promise<A> {
    const response = await fetchImpl(`${origin}${path}`, {
      method: "GET",
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) throw await decodeFailure(response);
    return Schema.decodeUnknownSync(schema)(await response.json());
  }

  async function submit<A>(
    schema: Schema.Codec<A, any, never, never>,
    path: string,
    csrf: string,
    fields: Record<string, string>,
  ): Promise<A> {
    const response = await fetchImpl(`${origin}${path}`, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/x-www-form-urlencoded",
        "X-CSRF-Token": csrf,
      },
      body: new URLSearchParams({ ...fields, csrf }).toString(),
    });
    if (!response.ok) throw await decodeFailure(response);
    return Schema.decodeUnknownSync(schema)(await response.json());
  }

  async function post<A>(schema: Schema.Codec<A, any, never, never>, path: string, csrf: string, body: unknown, method = "POST"): Promise<A> {
    const response = await fetchImpl(`${origin}${path}`, {
      method,
      credentials: "same-origin",
      headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify(body),
    });
    if (!response.ok) throw await decodeFailure(response);
    return Schema.decodeUnknownSync(schema)(await response.json());
  }

  const entitlementsPath = (organization: string) =>
    `/api/cloud/platform/organizations/${encodeURIComponent(organization)}/entitlements`;

  return {
    organizations: () => read(EntryOrganizations, "/api/cloud/organizations"),
    session: () => read(EntrySession, "/api/cloud/session"),
    provisioning: (organization: string) =>
      read(Provisioning, `/api/cloud/organizations/${encodeURIComponent(organization)}/provisioning`),
    createOrganization: (input: { name: string; key: string; csrf: string }) =>
      submit(NextResult, "/organizations", input.csrf, { name: input.name, creation_key: input.key }),
    resume: (input: { organization: string; csrf: string }) =>
      submit(NextResult, `/organizations/${encodeURIComponent(input.organization)}/provisioning/resume`, input.csrf, {}),
    platformOrganizations: () => read(PlatformOrganizations, "/api/cloud/platform/organizations"),
    platformTenant: (organization: string) => read(PlatformTenantDetail, `/api/cloud/platform/organizations/${encodeURIComponent(organization)}`),
    resumePlatformTenant: (input: { organization: string; csrf: string }) =>
      submit(NextResult, `/api/cloud/platform/organizations/${encodeURIComponent(input.organization)}/resume`, input.csrf, {}),
    platformAllowlist: () => read(PlatformAllowlist, "/api/cloud/platform/allowlist"),
    platformMembers: () => read(PlatformMembers, "/api/cloud/platform/members"),
    changePlatformMember: (input: { csrf: string; change: PlatformMemberChange }) => {
      const { action, email, ...body } = input.change;
      return post(PlatformMemberChangeResult,
        "/api/cloud/platform/members" + (action === "add" ? "" : `/${encodeURIComponent(email)}`),
        input.csrf, action === "add" ? { ...body, email } : body,
        action === "add" ? "POST" : action === "change" ? "PATCH" : "DELETE");
    },
    platformHealth: () => read(PlatformHealth, "/api/cloud/platform/health"),
    platformAccounts: (query: string) => read(PlatformAccounts, `/api/cloud/platform/accounts?q=${encodeURIComponent(query)}`),
    platformAudit: (query: string) => read(PlatformAudit, `/api/cloud/platform/audit${query === "" ? "" : `?${query}`}`),
    platformEntitlements: (organization: string) => read(OrganizationEntitlements, entitlementsPath(organization)),
    changePlatformEntitlement: (input: { organization: string; csrf: string; change: EntitlementChange }) =>
      post(EntitlementChangeResult, entitlementsPath(input.organization), input.csrf, input.change),
  };
}

export type EntryApi = ReturnType<typeof makeEntryApi>;

/** The provisioning checkpoints in order, for the progress list. */
export const PROVISIONING_STEPS = [
  { id: "admission", label: "Reserving capacity" },
  { id: "provider_organization", label: "Creating the organization" },
  { id: "owner_membership", label: "Making you the owner" },
  { id: "tenant_files", label: "Preparing storage" },
  { id: "tenant_start", label: "Starting your Hub" },
  { id: "owner_bootstrap", label: "Adding your account" },
  { id: "publish", label: "Opening the organization" },
] as const;

/** Index of the next checkpoint after the last completed one. */
export function currentStepIndex(completed: string): number {
  const index = PROVISIONING_STEPS.findIndex((step) => step.id === completed);
  return index < 0 ? 0 : Math.min(index + 1, PROVISIONING_STEPS.length - 1);
}

export function provisioningActive(state: string): boolean {
  return state === "requested" || state === "allocating";
}
