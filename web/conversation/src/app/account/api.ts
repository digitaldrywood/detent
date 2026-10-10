import { AccessKeys, CreatedAccessKey, KeyContext, KeyPolicy, type CreateAccessKey, type PersonalKeyPolicy } from "../../contracts/accessKeys.ts";
import { CloudModelSelection, type CloudSelection } from "../../contracts/account.ts";
import { IssueIntake, type IntakeCommand } from "../../contracts/githubIntake.ts";
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
  CreatedOperatorAPIKey,
  OperatorAPIKeys,
  BillingReport,
  CheckoutResponse,
  CreateOrganizationResponse,
  FleetResponse,
  FleetNamesResponse,
  Member,
  MembersResponse,
  NextResponse,
  Onboarding,
  OnboardingProgress,
  PlanReport,
  PolicyApproval,
  ProjectIntegration,
  ProjectSecretStatus,
  SlackIntegrationStatus,
  SpritePool,
  ProjectsResponse,
  OrganizationProjectRank,
  type ProjectGrant,
  RunnerEnrollment,
  SupportResponse,
  type WorkflowState,
} from "../../contracts/account.ts";
import { isApiError } from "../../contracts/index.ts";
import { hubPath } from "../../runtime/basePath.ts";
import { WorkAttachment } from "../../contracts/workAttachments.ts";
import { DiagnosticsReport, HealthFindingsRead } from "../../contracts/diagnostics.ts";
import { ActivityReport, type ActivityFilters } from "../../contracts/activity.ts";
import { ReportsReport, type ReportsRange } from "../../contracts/reports.ts";
import { clearBoardCache } from "../work/lib/boardStore.ts";

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
    if (body !== undefined && !(body instanceof FormData)) headers["Content-Type"] = "application/json";
    if (MUTATION_METHODS.has(method)) headers["X-CSRF-Token"] = options.csrfToken;
    let response: Response;
    try {
      response = await doFetch(`${options.origin}${path}`, {
        method,
        credentials: "same-origin",
        headers,
        ...(body === undefined ? {} : { body: body instanceof FormData ? body : JSON.stringify(body) }),
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

  const organizationId = base.split("/organizations/")[1]?.split("/")[0] ?? "";
  const cloudOrganization = `/api/cloud/organizations/${organizationId}`;
  return {
    personalKeys: () => send(AccessKeys, "GET", "/api/cloud/account/api-keys"),
    keyContext: () => send(KeyContext, "GET", "/api/cloud/account/key-context"),
    createPersonalKey: (input: CreateAccessKey) => send(CreatedAccessKey, "POST", "/api/cloud/account/api-keys", input),
    rotatePersonalKey: (id: string) => send(CreatedAccessKey, "POST", `/api/cloud/account/api-keys/${encodeURIComponent(id)}/rotate`, {}),
    revokePersonalKey: (id: string) => send(null, "DELETE", `/api/cloud/account/api-keys/${encodeURIComponent(id)}`),
    serviceKeys: () => send(AccessKeys, "GET", `${cloudOrganization}/service-keys`),
    createServiceKey: (input: CreateAccessKey) => send(CreatedAccessKey, "POST", `${cloudOrganization}/service-keys`, input),
    revokeServiceKey: (id: string) => send(null, "DELETE", `${cloudOrganization}/service-keys/${encodeURIComponent(id)}`),
    memberKeys: () => send(AccessKeys, "GET", `${cloudOrganization}/external-keys`),
    keyPolicy: () => send(KeyPolicy, "GET", `${cloudOrganization}/key-policy`),
    saveKeyPolicy: (policy: PersonalKeyPolicy) => send(KeyPolicy, "PUT", `${cloudOrganization}/key-policy`, { personal_keys: policy }),
    blockMemberKey: (id: string) => send(null, "PUT", `${cloudOrganization}/external-keys/${encodeURIComponent(id)}/block`, {}),
    approveMemberKey: (id: string) => send(null, "PUT", `${cloudOrganization}/external-keys/${encodeURIComponent(id)}/approve`, {}),
    apiKeys: () => send(OperatorAPIKeys, "GET", `${base}/api-keys`),
    createAPIKey: (input: { name: string; scope: string; expires_days: number; never_expires?: boolean; project_access?: "all" | "selected"; project_ids?: readonly string[] }) =>
      send(CreatedOperatorAPIKey, "POST", `${base}/api-keys`, input),
    revokeAPIKey: (id: string) => send(null, "DELETE", `${base}/api-keys/${encodeURIComponent(id)}`),
    origin: options.origin,
    apiBase: base,
    csrfToken: options.csrfToken,
    attachmentUrl: (projectId: string, id: string) => `${project(projectId)}/attachments/${encodeURIComponent(id)}`,
    attachment: (projectId: string, id: string) =>
      send(WorkAttachment, "GET", `${project(projectId)}/attachments/${encodeURIComponent(id)}/metadata`),
    uploadWorkAttachment: (projectId: string, file: File) => {
      const body = new FormData();
      body.append("file", file);
      return send(WorkAttachment, "POST", `${project(projectId)}/attachments`, body);
    },

    // --- Organization -------------------------------------------------------
    modelSelection: (projectId?: string) => send(CloudModelSelection, "GET", `${projectId === undefined ? base : project(projectId)}/model-selection`),
    saveModelSelection: (input: { projectId?: string; revision: string; selection: CloudSelection | null; key: string }) =>
      send(CloudModelSelection, "PUT", `${input.projectId === undefined ? base : project(input.projectId)}/model-selection`, {
        expected_revision: input.revision, selection: input.selection, idempotency_key: input.key,
      }),
    slackIntegration: () => send(SlackIntegrationStatus, "GET", `${base}/integrations/slack`),
    saveSlackIntegration: (input: { webhook?: string; channel_name: string }) => send(SlackIntegrationStatus, "PUT", `${base}/integrations/slack`, input),
    testSlackIntegration: () => send(SlackIntegrationStatus, "POST", `${base}/integrations/slack/test`, {}),
    members: () => send(MembersResponse, "GET", `${base}/members`),
    invite: (input: { email: string; role: string; key: string; grants?: readonly ProjectGrant[] }) =>
      send(Schema.Unknown, "POST", `${base}/members/invitations`, {
        email: input.email,
        role: input.role,
        grants: input.grants ?? [],
        idempotency_key: input.key,
      }),
    setInvitationGrants: (input: { invitation: string; grants: readonly ProjectGrant[]; key: string }) =>
      send(null, "PUT", `${base}/members/invitations/${encodeURIComponent(input.invitation)}`, {
        grants: input.grants,
        idempotency_key: input.key,
      }),
    revokeInvitation: (input: { invitation: string; key: string }) =>
      send(
        null,
        "DELETE",
        `${base}/members/invitations/${encodeURIComponent(input.invitation)}`,
        { idempotency_key: input.key },
      ),
    resendInvitation: (input: { invitation: string; key: string }) =>
      send(
        null,
        "POST",
        `${base}/members/invitations/${encodeURIComponent(input.invitation)}/resend`,
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
    switchOrganization: async (input: { organization: string; key: string }) => {
      const next = await send(NextResponse, "POST", `${base}/switch`, {
        organization: input.organization,
        idempotency_key: input.key,
      });
      clearBoardCache();
      return next;
    },
    startSupport: (input: { key: string }) =>
      send(SupportResponse, "POST", `${base}/support/start`, { idempotency_key: input.key }),
    /** `POST /logout` answers 204 for a JSON caller (§12, "Serving"). */
    logout: async () => {
      await send(null, "POST", hubPath("/logout"), {});
      clearBoardCache();
    },

    // --- Projects -----------------------------------------------------------
    projectRank: () => send(OrganizationProjectRank, "GET", `${base}/project-rank`),
    updateProjectRank: (input: { expectedRevision: number; projectIds: readonly string[] }) => send(OrganizationProjectRank, "PUT", `${base}/project-rank`, { expected_revision: input.expectedRevision, project_ids: input.projectIds }),
    projects: () => send(ProjectsResponse, "GET", `${base}/projects`),
    createProject: (input: { name: string; grantAccess: boolean; key: string }) =>
      send(Schema.Unknown, "POST", `${base}/projects`, {
        name: input.name,
        grant_access: input.grantAccess,
        idempotency_key: input.key,
      }),

    // --- Project settings ---------------------------------------------------
    spritesSecret: (projectId: string) =>
      send(ProjectSecretStatus, "GET", `${projectId ? project(projectId) : base}/secrets/fly_sprites_token`),
    spritePool: (_projectId: string) =>
      send(SpritePool, "GET", `${base}/sprite-pool`),
    setSpritePool: (_projectId: string, settings: { min_runners: number; max_runners: number; idle_seconds: number; bootstrap: string; revision: number }) =>
      send(SpritePool, "PUT", `${base}/sprite-pool`, settings),
    setSpritesSecret: (projectId: string, token: string) =>
      send(ProjectSecretStatus, "PUT", `${projectId ? project(projectId) : base}/secrets/fly_sprites_token`, { token }),
    removeSpritesSecret: (projectId: string) =>
      send(ProjectSecretStatus, "DELETE", `${projectId ? project(projectId) : base}/secrets/fly_sprites_token`),
    integration: (projectId: string) =>
      send(ProjectIntegration, "GET", `${project(projectId)}/integration`),
    deleteProject: (input: { projectId: string; name: string; key: string }) =>
      send(null, "DELETE", project(input.projectId), { idempotency_key: input.key, confirm_name: input.name }),
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
      intake?: string;
      projection: string;
      repositoryEnabled: boolean;
      archiveCompletedAfterDays?: number | null;
      archiveCancelledAfterDays?: number | null;
      states?: readonly WorkflowState[];
      workflowMarkdown?: string;
    }) =>
      send(ProjectIntegration, "PUT", `${project(input.projectId)}/onboarding/integration`, {
        expected_revision: input.revision,
        ...(input.intake !== undefined ? { intake: input.intake } : {}),
        projection: input.projection,
        repository_enabled: input.repositoryEnabled,
        ...(input.archiveCompletedAfterDays !== undefined ? { archive_completed_after_days: input.archiveCompletedAfterDays } : {}),
        ...(input.archiveCancelledAfterDays !== undefined ? { archive_cancelled_after_days: input.archiveCancelledAfterDays } : {}),
        ...(input.states !== undefined ? { states: input.states } : {}),
        ...(input.workflowMarkdown !== undefined ? { workflow_markdown: input.workflowMarkdown } : {}),
        idempotency_key: input.key,
      }),
    policy: (projectId: string, after?: string) => send(PolicyApproval, "GET", `${project(projectId)}/policy${after ? `?after=${encodeURIComponent(after)}` : ""}`),
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
    issueIntake: (projectId: string) => send(IssueIntake, "GET", `${project(projectId)}/onboarding/issue-intake`),
    commandIssueIntake: (projectId: string, command: IntakeCommand, key: string) => send(IssueIntake, "POST", `${project(projectId)}/onboarding/issue-intake`, { ...command, idempotency_key: key }),
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
	  githubIssueUrl?: string;
      title: string;
      body: string;
      state: string;
      key: string;
    }) =>
      send(Schema.Unknown, "POST", `${project(input.projectId)}/work-items`, {
	    github_issue_url: input.githubIssueUrl,
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
     * mutation and would refuse the field. Without runner and machine IDs the
     * token binds to whatever fresh IDs the host presents when it redeems it
     * (`detent hub runner register`); with them, a retry of the same
     * identifiers is made safe by the hub's identity collision (409).
     */
    enrollRunner: (input: {
      scope?: "projects" | "organization";
      projectIds: readonly string[];
      runnerId?: string;
      machineId?: string;
      operations?: readonly string[];
      ttlSeconds?: number;
    }) =>
      send(RunnerEnrollment, "POST", `${base}/runner-enrollments`, {
        ...(input.runnerId === undefined ? {} : { runner_id: input.runnerId }),
        ...(input.machineId === undefined ? {} : { machine_id: input.machineId }),
        scope: input.scope ?? "projects",
        project_ids: [...input.projectIds],
        operations: [
          ...(input.operations ?? ["read", "collaborate", "claim", "heartbeat", "events"]),
        ],
        ttl_seconds: input.ttlSeconds ?? 900,
      }),
    removeRunner: (runner: string) => send(Empty, "DELETE", `${base}/runners/${encodeURIComponent(runner)}`),
    runners: () => send(Schema.Array(Schema.Unknown), "GET", `${base}/runners`),
    /**
     * Routing is a read-modify-write: only the tags come from the form, and
     * the revision is re-read immediately before the write because a hosted
     * conflict cannot be recovered from its own response.
     */
    setRunnerRouting: (input: {
      scope?: "projects" | "organization";
      projectRanks?: Readonly<Record<string, number>>;
      runner: string;
      revision: number;
      displayName: string;
      tags: readonly string[];
      state: string;
      capacityLimit: number;
      projectIds: readonly string[];
      isolationTier?: string;
      hostServices?: readonly string[];
      availability?: { timezone: string; windows: readonly string[]; hard_deadline: string };
    }) =>
      send(Schema.Unknown, "PUT", `${base}/runners/${encodeURIComponent(input.runner)}/routing`, {
        expected_revision: input.revision,
        display_name: input.displayName,
        tags: input.tags,
        state: input.state,
        capacity_limit: input.capacityLimit,
        project_ids: input.projectIds,
        ...(input.scope !== undefined ? { scope: input.scope } : {}),
        ...(input.projectRanks !== undefined ? { project_ranks: input.projectRanks } : {}),
        ...(input.isolationTier !== undefined ? { isolation_tier: input.isolationTier } : {}),
        ...(input.hostServices !== undefined ? { host_services: input.hostServices } : {}),
        ...(input.availability !== undefined ? { availability: input.availability } : {}),
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
        source: "runner_checkout",
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
    activity: (filters: ActivityFilters = {}) => {
      const params = new URLSearchParams();
      for (const [key, value] of Object.entries(filters)) {
        if (value !== undefined) params.set(key, String(value));
      }
      return send(ActivityReport, "GET", `${base}/activity?${params.toString()}`);
    },
    fleet: () => send(FleetResponse, "GET", `${base}/fleet`),
    diagnostics: (range: string) => send(DiagnosticsReport, "GET", `${base}/diagnostics?range=${encodeURIComponent(range)}`),
    reports: (project: string, range: ReportsRange) => send(ReportsReport, "GET", `${base}/reports?project=${encodeURIComponent(project)}&range=${encodeURIComponent(range)}`),
    healthFindings: async (projects: readonly string[], options: { state?: "open" | "resolved"; since?: string } = {}): Promise<HealthFindingsRead> => {
      const reads = await Promise.all(projects.map(async (project) => {
        const items: HealthFindingsRead["items"][number][] = [];
        let cursor = "";
        let lastTick: string | null = null;
        do {
          const page = await send(HealthFindingsRead, "GET", `${base}/projects/${encodeURIComponent(project)}/health/findings?state=${options.state ?? "open"}${options.since ? `&since=${encodeURIComponent(options.since)}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
          items.push(...page.items);
          lastTick = page.last_tick_at;
          cursor = page.next_cursor ?? "";
        } while (cursor);
        return { items, last_tick_at: lastTick };
      }));
      const ticks = reads.map((read) => read.last_tick_at).filter((tick): tick is string => tick !== null);
      return {
        items: [...new Map(reads.flatMap((read) => read.items).map((finding) => [finding.id, finding])).values()],
        last_tick_at: ticks.length > 0 && ticks.length === reads.length
          ? ticks.toSorted((left, right) => Date.parse(left) - Date.parse(right))[0]!
          : null,
      };
    },
    fleetNames: () => send(FleetNamesResponse, "GET", `${base}/fleet?include=names`),
    plan: () => send(PlanReport, "GET", `${base}/plan`),
    billing: () => send(BillingReport, "GET", `${base}/billing`),
    checkout: (input: { price: string; key: string }) =>
      send(CheckoutResponse, "POST", `${base}/billing/checkout`, {
        price: input.price,
        idempotency_key: input.key,
      }),
    creditCheckout: (input: { price: string; key: string }) =>
      send(CheckoutResponse, "POST", `${base}/billing/credits/checkout`, {
        price: input.price, idempotency_key: input.key,
      }),
    creditAutoFund: (input: { enabled: boolean; threshold_cents: number; price: string }) =>
      send(Empty, "PUT", `${base}/billing/credits/auto-fund`, input),
    portal: (input: { key: string }) =>
      send(CheckoutResponse, "POST", `${base}/billing/portal`, { idempotency_key: input.key }),
  };
}

export type AccountApi = ReturnType<typeof makeAccountApi>;
export { Empty };
