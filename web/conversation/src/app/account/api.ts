// The hosted account API client.
//
// `runtime/rpc/http.ts` is the conversation client's own transport and stays
// that way: this is the same shape for the account surface of
// `docs/conversation/decisions.md` §12 — cookie session, `X-CSRF-Token` on
// every mutation, an `idempotency_key` in every non-GET body, and the native
// `{code, message, details?}` error decoded into one typed failure. It returns
// promises rather than effects because these screens are ordinary React data
// loads with no stream behind them.
import * as Schema from "effect/Schema";

import {
  BillingReport,
  CheckoutResponse,
  CreateOrganizationResponse,
  FleetResponse,
  Member,
  MembersResponse,
  NextResponse,
  Onboarding,
  OnboardingProgress,
  PlanReport,
  PolicyApproval,
  ProjectIntegration,
  ProjectsResponse,
  RunnerEnrollment,
  SupportResponse,
} from "../../contracts/account.ts";
import { isApiError } from "../../contracts/index.ts";
import { hubPath } from "../../runtime/basePath.ts";

/** A decoded failure from the hosted API, or the network under it. */
export class AccountError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: unknown;

  constructor(input: { status: number; code: string; message: string; details?: unknown }) {
    super(input.message);
    this.name = "AccountError";
    this.status = input.status;
    this.code = input.code;
    this.details = input.details ?? null;
  }

  /** True for the two statuses every screen turns into a friendly state. */
  get isAccessError(): boolean {
    return this.status === 403 || this.status === 404;
  }

  get isConflict(): boolean {
    return this.status === 409;
  }
}

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface AccountApiOptions {
  /** Origin the API is served from. Empty string for same-origin. */
  readonly origin: string;
  /** `api_base` from the bootstrap payload, e.g. `/api/v2/organizations/org_1`. */
  readonly apiBase: string;
  readonly csrfToken: string;
  readonly fetch?: FetchLike;
}

const MUTATION_METHODS = new Set(["POST", "PATCH", "PUT", "DELETE"]);

function messageForStatus(status: number): string {
  if (status === 401) return "Your session has expired. Sign in again.";
  if (status === 403) return "You do not have permission to do that.";
  if (status === 404) return "That is not available on this organization.";
  if (status === 409) return "Somebody else changed this first.";
  if (status >= 500) return "The hub could not complete the request.";
  return `The request failed (${status}).`;
}

function failure(status: number, body: unknown): AccountError {
  if (isApiError(body)) {
    const error = body as { code: string; message: string; details?: unknown };
    return new AccountError({
      status,
      code: error.code,
      message: error.message.length > 0 ? error.message : messageForStatus(status),
      details: error.details ?? null,
    });
  }
  return new AccountError({
    status,
    code: status === 404 ? "not_found" : status === 403 ? "forbidden" : "invalid",
    message: messageForStatus(status),
  });
}

/** A decoder that accepts a 204 with no body. */
const Empty = Schema.Struct({});

export function makeAccountApi(options: AccountApiOptions) {
  const doFetch: FetchLike = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  const base = options.apiBase;

  async function send<A>(
    schema: Schema.Codec<A, any, never, never> | null,
    method: string,
    path: string,
    body?: unknown,
  ): Promise<A> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (MUTATION_METHODS.has(method)) headers["X-CSRF-Token"] = options.csrfToken;
    let response: Response;
    try {
      response = await doFetch(`${options.origin}${path}`, {
        method,
        credentials: "same-origin",
        headers,
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
    } catch (cause) {
      throw new AccountError({
        status: 0,
        code: "network",
        message: cause instanceof Error ? cause.message : String(cause),
      });
    }
    const text = await response.text();
    let parsed: unknown = undefined;
    if (text.length > 0) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = undefined;
      }
    }
    if (!response.ok) throw failure(response.status, parsed);
    if (schema === null) return undefined as A;
    try {
      return Schema.decodeUnknownSync(schema)(parsed ?? {});
    } catch (cause) {
      throw new AccountError({
        status: response.status,
        code: "invalid",
        message: `The hub returned an unexpected payload: ${
          cause instanceof Error ? cause.message : String(cause)
        }`,
      });
    }
  }

  const project = (projectId: string) => `${base}/projects/${encodeURIComponent(projectId)}`;

  return {
    origin: options.origin,
    apiBase: base,
    csrfToken: options.csrfToken,

    // --- Organization -------------------------------------------------------
    members: () => send(MembersResponse, "GET", `${base}/members`),
    invite: (input: { email: string; role: string; key: string }) =>
      send(Schema.Unknown, "POST", `${base}/members/invitations`, {
        email: input.email,
        role: input.role,
        idempotency_key: input.key,
      }),
    revokeInvitation: (input: { invitation: string; key: string }) =>
      send(
        null,
        "DELETE",
        `${base}/members/invitations/${encodeURIComponent(input.invitation)}`,
        { idempotency_key: input.key },
      ),
    removeMember: (input: { member: string; key: string }) =>
      send(null, "DELETE", `${base}/members/${encodeURIComponent(input.member)}`, {
        idempotency_key: input.key,
      }),
    setMemberRole: (input: { member: string; role: string; key: string }) =>
      send(Member, "PUT", `${base}/members/${encodeURIComponent(input.member)}/role`, {
        role: input.role,
        idempotency_key: input.key,
      }),
    setMemberGrant: (input: {
      member: string;
      projectId: string;
      write: boolean;
      runner: boolean;
      revoke?: boolean;
      key: string;
    }) =>
      send(Member, "PUT", `${base}/members/${encodeURIComponent(input.member)}/grants`, {
        project_id: input.projectId,
        write: input.write,
        runner: input.runner,
        revoke: input.revoke ?? false,
        idempotency_key: input.key,
      }),
    createOrganization: (input: { name: string; key: string }) =>
      send(CreateOrganizationResponse, "POST", `/api/v2/organizations`, {
        name: input.name,
        idempotency_key: input.key,
      }),
    acceptInvitation: (input: { token: string; key: string }) =>
      send(NextResponse, "POST", `${base}/invitations/accept`, {
        token: input.token,
        idempotency_key: input.key,
      }),
    switchOrganization: (input: { organization: string; key: string }) =>
      send(NextResponse, "POST", `${base}/switch`, {
        organization: input.organization,
        idempotency_key: input.key,
      }),
    startSupport: (input: { key: string }) =>
      send(SupportResponse, "POST", `${base}/support/start`, { idempotency_key: input.key }),
    /** `POST /logout` answers 204 for a JSON caller (§12, "Serving"). */
    logout: () => send(null, "POST", hubPath("/logout"), {}),

    // --- Projects -----------------------------------------------------------
    projects: () => send(ProjectsResponse, "GET", `${base}/projects`),
    createProject: (input: { name: string; grantAccess: boolean; key: string }) =>
      send(Schema.Unknown, "POST", `${base}/projects`, {
        name: input.name,
        grant_access: input.grantAccess,
        idempotency_key: input.key,
      }),

    // --- Project settings ---------------------------------------------------
    integration: (projectId: string) =>
      send(ProjectIntegration, "GET", `${project(projectId)}/integration`),
    /**
     * `expected_revision` is the revision the screen was showing, sent back as
     * the string the hub marshalled. In hosted mode a `409` carries no current
     * revision (`nativeAPIError` redacts it), so the caller re-reads rather
     * than retrying with a guess.
     */
    saveIntegration: (input: {
      projectId: string;
      key: string;
      revision: string;
      intake: string;
      projection: string;
      repositoryEnabled: boolean;
    }) =>
      send(ProjectIntegration, "PUT", `${project(input.projectId)}/integration`, {
        expected_revision: input.revision,
        intake: input.intake,
        projection: input.projection,
        repository_enabled: input.repositoryEnabled,
        idempotency_key: input.key,
      }),
    policy: (projectId: string) => send(PolicyApproval, "GET", `${project(projectId)}/policy`),
    /**
     * Approval is by identity: the descriptor the reader was shown goes back
     * verbatim, and `expected_policy_id` is the approval it replaces (empty on
     * the first one). No idempotency key: the handler is not a native
     * mutation and `DisallowUnknownFields` would refuse the field.
     */
    approvePolicy: (input: {
      projectId: string;
      expectedPolicyId: string;
      policy: unknown;
      onboarding?: boolean;
    }) =>
      send(
        PolicyApproval,
        "PUT",
        `${project(input.projectId)}${input.onboarding === true ? "/onboarding" : ""}/policy`,
        { expected_policy_id: input.expectedPolicyId, policy: input.policy },
      ),

    // --- Onboarding (the first-run wizard) ----------------------------------
    onboarding: (projectId: string) => send(Onboarding, "GET", `${project(projectId)}/onboarding`),
    /**
     * The progress save. The response is the stored `Progress` alone, with the
     * revision already incremented, which is what the wizard carries into the
     * next step rather than re-reading the whole project.
     */
    saveProgress: (input: {
      projectId: string;
      key: string;
      revision: string;
      repository: string;
      doctor: boolean;
      provider: boolean;
      artifacts: string;
    }) =>
      send(OnboardingProgress, "PUT", `${project(input.projectId)}/onboarding`, {
        progress: {
          revision: input.revision,
          repository: input.repository,
          doctor: input.doctor,
          provider: input.provider,
          artifacts: input.artifacts,
        },
        idempotency_key: input.key,
      }),
    createFirstIssue: (input: {
      projectId: string;
      title: string;
      body: string;
      state: string;
      key: string;
    }) =>
      send(Schema.Unknown, "POST", `${project(input.projectId)}/work-items`, {
        title: input.title,
        body: input.body,
        state: input.state,
        labels: [],
        assignees: [],
        idempotency_key: input.key,
      }),
    /**
     * 201, and the only response the caller has to keep on screen: the token
     * is shown once and the hub stores only its hash.
     *
     * No idempotency key, because `POST /runner-enrollments` is not a native
     * mutation and would refuse the field. A retry of the same identifiers is
     * made safe by the hub's identity collision (409) instead.
     */
    enrollRunner: (input: {
      projectIds: readonly string[];
      runnerId: string;
      machineId: string;
      operations?: readonly string[];
      ttlSeconds?: number;
    }) =>
      send(RunnerEnrollment, "POST", `${base}/runner-enrollments`, {
        runner_id: input.runnerId,
        machine_id: input.machineId,
        project_ids: [...input.projectIds],
        operations: [
          ...(input.operations ?? ["read", "collaborate", "claim", "heartbeat", "events"]),
        ],
        ttl_seconds: input.ttlSeconds ?? 900,
      }),
    runners: () => send(Schema.Array(Schema.Unknown), "GET", `${base}/runners`),
    /**
     * Routing is a read-modify-write: only the tags come from the form, and
     * the revision is re-read immediately before the write because a hosted
     * conflict cannot be recovered from its own response.
     */
    setRunnerRouting: (input: {
      runner: string;
      revision: number;
      displayName: string;
      tags: readonly string[];
      state: string;
      capacityLimit: number;
      projectIds: readonly string[];
    }) =>
      send(Schema.Unknown, "PUT", `${base}/runners/${encodeURIComponent(input.runner)}/routing`, {
        expected_revision: input.revision,
        display_name: input.displayName,
        tags: input.tags,
        state: input.state,
        capacity_limit: input.capacityLimit,
        project_ids: input.projectIds,
      }),
    bindRepository: (input: {
      projectId: string;
      repository: string;
      revision: string;
      key: string;
    }) =>
      send(ProjectIntegration, "POST", `${project(input.projectId)}/onboarding/repository`, {
        expected_revision: input.revision,
        repository: input.repository,
        idempotency_key: input.key,
      }),
    bindArtifactService: (input: {
      projectId: string;
      serviceId: string;
      origin: string;
      publisherTokenId: string;
    }) =>
      send(
        Schema.Unknown,
        "PUT",
        `${project(input.projectId)}/onboarding/artifact-services/${encodeURIComponent(input.serviceId)}`,
        {
          service_id: input.serviceId,
          origin: input.origin,
          mode: "customer",
          publisher_token_id: input.publisherTokenId,
        },
      ),

    // --- Fleet, plan and billing --------------------------------------------
    fleet: () => send(FleetResponse, "GET", `${base}/fleet`),
    plan: () => send(PlanReport, "GET", `${base}/plan`),
    billing: () => send(BillingReport, "GET", `${base}/billing`),
    checkout: (input: { price: string; key: string }) =>
      send(CheckoutResponse, "POST", `${base}/billing/checkout`, {
        price: input.price,
        idempotency_key: input.key,
      }),
    portal: (input: { key: string }) =>
      send(CheckoutResponse, "POST", `${base}/billing/portal`, { idempotency_key: input.key }),
  };
}

export type AccountApi = ReturnType<typeof makeAccountApi>;
export { Empty };
