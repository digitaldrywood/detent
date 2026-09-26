// The shared entry's screens at `https://hub.detent.build`: sign-in, the
// organization chooser, creation with its provisioning progress, and joining
// with an invitation token. They sit outside every organization's base path,
// so links here are plain root paths and opening an organization is a full
// navigation into that organization's own client.
import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { DetentCloudLogo, LoginCard, locationError } from "../account/Login.tsx";
import { AccountError } from "../account/api.ts";
import { newKey } from "../account/idempotency.ts";
import { useMutation, useResource } from "../account/useResource.ts";
import {
  currentStepIndex,
  type EntryApi,
  type EntryOrganizations,
  makeEntryApi,
  PROVISIONING_STEPS,
  type Provisioning,
  provisioningActive,
  SIGN_IN_ORGANIZATIONS,
} from "./api.ts";

export const EntryApiContext = React.createContext<EntryApi>(makeEntryApi());

export function useEntryApi(): EntryApi {
  return React.useContext(EntryApiContext);
}

type Navigate = (to: string) => void;

function assign(to: string): void {
  globalThis.location?.assign(to);
}

function signInAgain(error: AccountError | null): boolean {
  if (error?.status !== 401) return false;
  assign(SIGN_IN_ORGANIZATIONS);
  return true;
}

function EntryCard({
  title,
  description,
  children,
}: {
  readonly title: string;
  readonly description?: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <div className="grid flex-1 place-items-center overflow-y-auto p-4 sm:p-10">
      <section className="w-full max-w-[640px] overflow-hidden rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]">
        <div className="border-b border-border/60 px-5 pt-[22px] pb-[16px] sm:px-7">
          <DetentCloudLogo />
        </div>
        <div className="px-5 pt-6 pb-[26px] sm:px-7">
          <h1 className="mb-2 text-2xl font-semibold tracking-[-0.02em] text-balance">{title}</h1>
          {description === undefined ? null : (
            <p className="mb-[18px] text-sm text-muted-foreground">{description}</p>
          )}
          {children}
        </div>
      </section>
    </div>
  );
}

function Problem({ message }: { readonly message: string | null }): React.ReactElement | null {
  if (message === null) return null;
  return (
    <div
      role="alert"
      className="mb-4 rounded-[10px] border border-destructive/30 bg-destructive/8 px-3.5 py-3 text-[13px] text-destructive-foreground"
    >
      {message}
    </div>
  );
}

function SignOut({ csrf }: { readonly csrf: string }): React.ReactElement {
  return (
    <form method="post" action="/logout">
      <input type="hidden" name="csrf" value={csrf} />
      <Button type="submit" variant="ghost" size="sm">
        Sign out
      </Button>
    </form>
  );
}

export function EntrySignIn(): React.ReactElement {
  return (
    <LoginCard
      error={locationError()}
      onAcceptInvitation={(token) =>
        assign(`/invite?invitation_token=${encodeURIComponent(token)}`)
      }
    />
  );
}

const STATE_LABELS: Record<string, string> = {
  requested: "Waiting to start",
  allocating: "Setting up",
  failed: "Needs attention",
};

export function OrganizationChooser({ onNavigate }: { readonly onNavigate: Navigate }): React.ReactElement {
  const api = useEntryApi();
  const listing = useResource<EntryOrganizations>(() => api.organizations(), [api]);
  if (signInAgain(listing.error)) return <EntryCard title="Signing you in…">{null}</EntryCard>;
  const value = listing.value;
  return (
    <EntryCard
      title="Choose an organization"
      description={value === undefined ? undefined : `Signed in as ${value.email}.`}
    >
      <Problem message={listing.error?.message ?? null} />
      {listing.loading && value === undefined ? (
        <p className="text-sm text-muted-foreground">Loading your organizations…</p>
      ) : null}
      {value === undefined ? null : (
        <div className="flex flex-col gap-5">
          {value.organizations.length === 0 && (value.pending ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">
              Your account does not belong to an organization yet. Create one, or use the invitation
              sent to you.
            </p>
          ) : null}
          {value.organizations.length === 0 ? null : (
            <ul className="flex flex-col gap-2" aria-label="Your organizations">
              {value.organizations.map((organization) => (
                <li key={organization.id}>
                  <a
                    href={organization.url}
                    className="flex min-h-11 items-center justify-between gap-3 rounded-[10px] border border-border px-4 text-sm font-medium hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    <span className="truncate">{organization.name}</span>
                    <span aria-hidden="true" className="text-muted-foreground">
                      →
                    </span>
                  </a>
                </li>
              ))}
            </ul>
          )}
          {(value.pending ?? []).length === 0 ? null : (
            <div>
              <h2 className="mb-2 text-sm font-semibold">Being set up</h2>
              <ul className="flex flex-col gap-2" aria-label="Organizations being set up">
                {(value.pending ?? []).map((organization) => (
                  <li key={organization.id}>
                    <button
                      type="button"
                      onClick={() => onNavigate(`/organizations/${organization.id}/provisioning`)}
                      className="flex min-h-11 w-full items-center justify-between gap-3 rounded-[10px] border border-border px-4 text-left text-sm hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    >
                      <span className="truncate">{organization.name}</span>
                      <Badge variant="outline">{STATE_LABELS[organization.state ?? ""] ?? organization.state}</Badge>
                    </button>
                  </li>
                ))}
              </ul>
            </div>
          )}
          <div className="flex flex-wrap items-center gap-2 border-t border-border/60 pt-4">
            {value.can_create === true ? (
              <Button onClick={() => onNavigate("/organizations/new")}>Create organization</Button>
            ) : null}
            <Button variant="outline" onClick={() => onNavigate("/invitations/join")}>
              Join with invitation
            </Button>
            <span className="flex-1" />
            <SignOut csrf={value.csrf} />
          </div>
        </div>
      )}
    </EntryCard>
  );
}

export function CreateOrganization({ onNavigate }: { readonly onNavigate: Navigate }): React.ReactElement {
  const api = useEntryApi();
  const listing = useResource<EntryOrganizations>(() => api.organizations(), [api]);
  const [name, setName] = React.useState("");
  // One key for this form: a retried submit after an uncertain response
  // returns the same organization instead of creating a second one.
  const key = React.useMemo(() => newKey(), []);
  const create = useMutation(async (organizationName: string) => {
    const csrf = listing.value?.csrf ?? "";
    const result = await api.createOrganization({ name: organizationName, key, csrf });
    onNavigate(result.next);
    return result;
  });
  if (signInAgain(listing.error ?? create.error)) return <EntryCard title="Signing you in…">{null}</EntryCard>;
  const trimmed = name.trim();
  return (
    <EntryCard
      title="Create an organization"
      description="You become its owner. The free plan applies, and no payment details are needed."
    >
      <Problem message={create.error?.message ?? listing.error?.message ?? null} />
      <form
        className="flex flex-col gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          if (trimmed.length === 0 || trimmed.length > 120) return;
          void create.call(trimmed);
        }}
      >
        <Label htmlFor="entry-organization-name" className="text-sm font-medium">
          Organization name
        </Label>
        <Input
          id="entry-organization-name"
          name="name"
          autoComplete="organization"
          maxLength={120}
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
        />
        <div className="flex flex-wrap gap-2">
          <Button
            type="submit"
            disabled={create.pending || listing.value === undefined || trimmed.length === 0}
          >
            {create.pending ? "Creating…" : "Create organization"}
          </Button>
          <Button type="button" variant="outline" onClick={() => onNavigate("/organizations")}>
            Back to organizations
          </Button>
        </div>
      </form>
    </EntryCard>
  );
}

export function ProvisioningProgress({
  organization,
  onNavigate,
  pollMs = 2_000,
}: {
  readonly organization: string;
  readonly onNavigate: Navigate;
  readonly pollMs?: number;
}): React.ReactElement {
  const api = useEntryApi();
  const status = useResource<Provisioning>(() => api.provisioning(organization), [api, organization]);
  const listing = useResource<EntryOrganizations>(() => api.organizations(), [api]);
  const value = status.value;
  const next = value?.next;
  const active = value !== undefined && provisioningActive(value.state);

  React.useEffect(() => {
    if (next !== undefined) assign(next);
  }, [next]);

  const refresh = status.refresh;
  React.useEffect(() => {
    if (!active) return;
    const timer = globalThis.setInterval(() => void refresh(), pollMs);
    return () => globalThis.clearInterval(timer);
  }, [active, refresh, pollMs]);

  const resume = useMutation(async () => {
    await api.resume({ organization, csrf: listing.value?.csrf ?? "" });
    await status.refresh();
  });

  if (signInAgain(status.error)) return <EntryCard title="Signing you in…">{null}</EntryCard>;
  const current = value === undefined ? 0 : currentStepIndex(value.step);
  return (
    <EntryCard
      title={value === undefined ? "Setting up your organization" : `Setting up ${value.name}`}
      description="This takes a moment. You can leave this page; setup continues and your organization appears in your list when it is ready."
    >
      <Problem message={status.error?.message ?? resume.error?.message ?? null} />
      {value === undefined ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : (
        <div className="flex flex-col gap-4">
          <ol className="flex flex-col gap-1.5" aria-label="Setup progress">
            {PROVISIONING_STEPS.map((step, index) => {
              const done = value.state === "ready" || index < current;
              const here = index === current && value.state !== "ready";
              return (
                <li
                  key={step.id}
                  aria-current={here ? "step" : undefined}
                  className={
                    done
                      ? "text-sm text-foreground"
                      : here
                        ? "text-sm font-medium text-foreground"
                        : "text-sm text-muted-foreground"
                  }
                >
                  <span aria-hidden="true" className="mr-2 inline-block w-4">
                    {done ? "✓" : here ? (value.state === "failed" ? "!" : "…") : "·"}
                  </span>
                  {step.label}
                </li>
              );
            })}
          </ol>
          <p role="status" className="text-sm">
            {value.state === "ready"
              ? "Ready. Opening your organization…"
              : value.state === "failed"
                ? "Setup stopped."
                : "Setting up…"}
          </p>
          {value.error === "" ? null : <Problem message={value.error} />}
          <div className="flex flex-wrap gap-2">
            {value.can_resume ? (
              <Button onClick={() => void resume.call()} disabled={resume.pending || listing.value === undefined}>
                {resume.pending ? "Resuming…" : "Resume setup"}
              </Button>
            ) : null}
            <Button variant="outline" onClick={() => onNavigate("/organizations")}>
              Back to organizations
            </Button>
          </div>
        </div>
      )}
    </EntryCard>
  );
}

export function JoinInvitation({ onNavigate }: { readonly onNavigate: Navigate }): React.ReactElement {
  const api = useEntryApi();
  const listing = useResource<EntryOrganizations>(() => api.organizations(), [api]);
  const [token, setToken] = React.useState("");
  const join = useMutation(async (value: string) => {
    const result = await api.joinInvitation({ token: value, csrf: listing.value?.csrf ?? "" });
    assign(result.next);
    return result;
  });
  if (signInAgain(listing.error)) return <EntryCard title="Signing you in…">{null}</EntryCard>;
  return (
    <EntryCard
      title="Join with invitation"
      description="Paste the invitation token sent to your verified email address."
    >
      <Problem message={join.error?.message ?? null} />
      <form
        className="flex flex-col gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          const value = token.trim();
          if (value.length > 0) void join.call(value);
        }}
      >
        <Label htmlFor="entry-invitation-token" className="text-sm font-medium">
          Invitation token
        </Label>
        <Input
          id="entry-invitation-token"
          name="token"
          type="password"
          autoComplete="off"
          value={token}
          onChange={(event) => setToken(event.currentTarget.value)}
        />
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={join.pending || token.trim().length === 0 || listing.value === undefined}>
            {join.pending ? "Joining…" : "Join organization"}
          </Button>
          <Button type="button" variant="outline" onClick={() => onNavigate("/organizations")}>
            Back to organizations
          </Button>
        </div>
      </form>
    </EntryCard>
  );
}
