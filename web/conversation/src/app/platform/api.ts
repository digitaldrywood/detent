// The platform console's reads and actions.
//
// Two kinds of endpoint sit behind one client. `live` ones are served by the
// entry today. `proposed` ones are the new API docs/platform-admin.md asks
// for: only the mock hub answers them, and outside preview mode the client
// never requests them, so a production console can never paint fixture data.
import * as Schema from "effect/Schema";

import { AccountError, type FetchLike } from "../account/api.ts";
import { makeEntryApi } from "../entry/api.ts";
import {
  ActionResult,
  AuditPage,
  OrganizationBilling,
  OrganizationDetail,
  OrganizationMembers,
  OrganizationProjects,
  OrganizationUsage,
  PlatformBilling,
  PlatformOverview,
  PlatformPlans,
  PlatformRunners,
  PlatformSettings,
  PlatformTenants,
  PlatformUsers,
  ProvisioningHistory,
  SupportSessions,
} from "../../contracts/platform.ts";

export const PLANNED_CODE = "planned_endpoint";

/** The error a proposed endpoint raises outside preview mode. */
export class PlannedEndpoint extends AccountError {
  readonly endpoint: string;
  constructor(endpoint: string) {
    super({ status: 0, code: PLANNED_CODE, message: `${endpoint} is not served yet.` });
    this.endpoint = endpoint;
  }
}

export function isPlanned(error: unknown): error is PlannedEndpoint {
  return error instanceof AccountError && error.code === PLANNED_CODE;
}

/** Preview mode is on only for `npm run dev:mock`, which sets the flag. */
export function platformPreview(): boolean {
  return import.meta.env.VITE_PLATFORM_PREVIEW === "1";
}

const BASE = "/api/cloud/platform";

function organizationPath(organization: string, rest = ""): string {
  return `${BASE}/organizations/${encodeURIComponent(organization)}${rest}`;
}

export interface AuditQuery {
  readonly organization?: string;
  readonly actorKind?: string;
  readonly q?: string;
}

export function makePlatformApi(
  options: { readonly fetch?: FetchLike; readonly origin?: string; readonly preview?: boolean } = {},
) {
  const fetchImpl: FetchLike = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  const origin = options.origin ?? "";
  const preview = options.preview ?? platformPreview();
  const entry = makeEntryApi({ fetch: fetchImpl, origin });

  async function failure(response: Response): Promise<AccountError> {
    const body = (await response.json().catch(() => null)) as { code?: unknown; message?: unknown } | null;
    return new AccountError({
      status: response.status,
      code: typeof body?.code === "string" ? body.code : "request_failed",
      message:
        typeof body?.message === "string"
          ? body.message
          : response.status >= 500
            ? "Detent could not complete the request. Try again shortly."
            : `The request failed (${response.status}).`,
    });
  }

  async function proposed<A>(
    schema: Schema.Codec<A, any, never, never>,
    path: string,
    init?: { readonly csrf: string; readonly body: unknown },
  ): Promise<A> {
    const method = init === undefined ? "GET" : "POST";
    if (!preview) throw new PlannedEndpoint(`${method} ${path.split("?")[0]}`);
    const response = await fetchImpl(`${origin}${path}`, {
      method,
      credentials: "same-origin",
      headers:
        init === undefined
          ? { Accept: "application/json" }
          : { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": init.csrf },
      ...(init === undefined ? {} : { body: JSON.stringify(init.body) }),
    });
    if (!response.ok) throw await failure(response);
    return Schema.decodeUnknownSync(schema)(await response.json());
  }

  const act = (path: string, csrf: string, body: Record<string, unknown>) =>
    proposed(ActionResult, path, { csrf, body });

  return {
    entry,
    live: {
      organizations: entry.platformOrganizations,
      allowlist: entry.platformAllowlist,
      health: entry.platformHealth,
      entitlements: entry.platformEntitlements,
      changeEntitlement: entry.changePlatformEntitlement,
    },
    overview: () => proposed(PlatformOverview, `${BASE}/overview`),
    organization: (id: string) => proposed(OrganizationDetail, organizationPath(id)),
    members: (id: string) => proposed(OrganizationMembers, organizationPath(id, "/members")),
    projects: (id: string) => proposed(OrganizationProjects, organizationPath(id, "/projects")),
    usage: (id: string) => proposed(OrganizationUsage, organizationPath(id, "/usage")),
    billing: (id: string) => proposed(OrganizationBilling, organizationPath(id, "/billing")),
    provisioning: (id: string) => proposed(ProvisioningHistory, organizationPath(id, "/provisioning")),
    runners: (organization?: string) =>
      proposed(
        PlatformRunners,
        organization === undefined ? `${BASE}/runners` : organizationPath(organization, "/runners"),
      ),
    audit: (query: AuditQuery = {}) => {
      const params = new URLSearchParams();
      if (query.organization) params.set("organization", query.organization);
      if (query.actorKind) params.set("actor_kind", query.actorKind);
      if (query.q) params.set("q", query.q);
      const search = params.toString();
      return proposed(AuditPage, `${BASE}/audit${search === "" ? "" : `?${search}`}`);
    },
    users: () => proposed(PlatformUsers, `${BASE}/users`),
    plans: () => proposed(PlatformPlans, `${BASE}/plans`),
    platformBilling: () => proposed(PlatformBilling, `${BASE}/billing`),
    tenants: () => proposed(PlatformTenants, `${BASE}/tenants`),
    supportSessions: () => proposed(SupportSessions, `${BASE}/support-sessions`),
    settings: () => proposed(PlatformSettings, `${BASE}/settings`),
    retryProvisioning: (input: { organization: string; csrf: string; reason: string }) =>
      act(organizationPath(input.organization, "/provisioning/retry"), input.csrf, { reason: input.reason }),
    suspend: (input: { organization: string; csrf: string; reason: string }) =>
      act(organizationPath(input.organization, "/suspend"), input.csrf, { reason: input.reason }),
    reactivate: (input: { organization: string; csrf: string; reason: string }) =>
      act(organizationPath(input.organization, "/reactivate"), input.csrf, { reason: input.reason }),
    endSupportSession: (input: { session: string; csrf: string; reason: string }) =>
      act(`${BASE}/support-sessions/${encodeURIComponent(input.session)}/end`, input.csrf, { reason: input.reason }),
    redeliverBillingEvent: (input: { event: string; csrf: string }) =>
      act(`${BASE}/billing/events/${encodeURIComponent(input.event)}/redeliver`, input.csrf, {}),
  };
}

export type PlatformApi = ReturnType<typeof makePlatformApi>;
