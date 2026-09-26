// The shared entry's JSON surface: the organization chooser, self-service
// creation and its provisioning status, and invitation joins. These live on
// the shared origin itself, outside any organization's base path, and use the
// entry session's CSRF token from the chooser payload.
import * as Schema from "effect/Schema";

import { AccountError, type FetchLike } from "../account/api.ts";

export const EntryOrganization = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  url: Schema.String,
  state: Schema.optional(Schema.String),
});
export type EntryOrganization = typeof EntryOrganization.Type;

export const EntryOrganizations = Schema.Struct({
  email: Schema.String,
  csrf: Schema.String,
  organizations: Schema.Array(EntryOrganization),
  pending: Schema.optional(Schema.Array(EntryOrganization)),
  can_create: Schema.optional(Schema.Boolean),
});
export type EntryOrganizations = typeof EntryOrganizations.Type;

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

export const SIGN_IN_ORGANIZATIONS = "/auth/oidc/start?return=%2Forganizations";

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

  return {
    organizations: () => read(EntryOrganizations, "/api/cloud/organizations"),
    provisioning: (organization: string) =>
      read(Provisioning, `/api/cloud/organizations/${encodeURIComponent(organization)}/provisioning`),
    createOrganization: (input: { name: string; key: string; csrf: string }) =>
      submit(NextResult, "/organizations", input.csrf, { name: input.name, creation_key: input.key }),
    resume: (input: { organization: string; csrf: string }) =>
      submit(NextResult, `/organizations/${encodeURIComponent(input.organization)}/provisioning/resume`, input.csrf, {}),
    joinInvitation: (input: { token: string; csrf: string }) =>
      submit(NextResult, "/invitations/join", input.csrf, { token: input.token }),
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
