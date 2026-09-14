// `/support`.
//
// The one screen a Detent staff account reaches before it has any access to
// the organization it is about to support, so — like `/login` — it renders
// without the bootstrap payload (decisions.md §12, "Serving"): the hub serves
// the shell for `/support` to anybody holding a session, and `/app/bootstrap`
// answers 403 until the support session has actually been started.
//
// It does one thing: `POST {apiBase}/support/start`, then reload, because
// every other screen reads the bootstrap and the bootstrap is what changes.
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { useAccountBootstrap } from "./context.ts";
import { DetentCloudLogo } from "./Login.tsx";
import { newKey } from "./idempotency.ts";

/**
 * Which organization the session is being started against. The path wins
 * (`/support/org_1`, or `?organization=org_1`); otherwise the refused
 * bootstrap's body is asked, because a 403 from `/app/bootstrap` is the one
 * response that already knows which organization this host serves.
 */
export function supportOrganization(input: {
  readonly pathname?: string;
  readonly search?: string;
  readonly body?: unknown;
}): string | null {
  const path = input.pathname ?? "";
  const segments = path.split("/").filter((segment) => segment.length > 0);
  if (segments[0] === "support" && typeof segments[1] === "string" && segments[1].length > 0) {
    return decodeURIComponent(segments[1]);
  }
  const query = new URLSearchParams(input.search ?? "").get("organization");
  if (query !== null && query.length > 0) return query;
  return organizationFromBody(input.body);
}

/** The `organization` a hub names in an error body, wherever it puts it. */
function organizationFromBody(body: unknown): string | null {
  if (body === null || typeof body !== "object") return null;
  const record = body as Record<string, unknown>;
  const direct = record["organization"];
  if (typeof direct === "string" && direct.length > 0) return direct;
  const details = record["details"];
  if (details !== null && typeof details === "object") {
    const nested = (details as Record<string, unknown>)["organization"];
    if (typeof nested === "string" && nested.length > 0) return nested;
  }
  return null;
}

export interface SupportCardProps {
  /** The organization the session will be started against, if one is known. */
  readonly organization: string | null;
  /** The CSRF token, where a bootstrap succeeded and carried one. */
  readonly csrfToken?: string | null;
  readonly fetchImpl?: typeof globalThis.fetch;
  /** What to do once the hub accepts. The default reloads the application. */
  readonly onStarted?: () => void;
}

/**
 * The card. It carries no dependency on the conversation client, so it renders
 * for a reader whose bootstrap was refused as readily as for one whose was not.
 */
export function SupportCard(props: SupportCardProps): React.ReactElement {
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const organization = props.organization;

  const start = async () => {
    if (organization === null) return;
    setPending(true);
    setError(null);
    const doFetch = props.fetchImpl ?? globalThis.fetch;
    try {
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Accept: "application/json",
      };
      if (props.csrfToken != null && props.csrfToken.length > 0) {
        headers["X-CSRF-Token"] = props.csrfToken;
      }
      const response = await doFetch(
        `/api/v2/organizations/${encodeURIComponent(organization)}/support/start`,
        {
          method: "POST",
          credentials: "same-origin",
          headers,
          body: JSON.stringify({ idempotency_key: newKey() }),
        },
      );
      if (!response.ok) {
        const body: unknown = await response.json().catch(() => undefined);
        const message =
          body !== null && typeof body === "object" && typeof (body as { message?: unknown }).message === "string"
            ? ((body as { message: string }).message)
            : `The hub refused the support session (${response.status}).`;
        setPending(false);
        setError(message);
        return;
      }
      // Every screen reads the bootstrap, and the bootstrap is what a started
      // support session changes, so the honest way in is to load it again.
      if (props.onStarted !== undefined) props.onStarted();
      else globalThis.location?.reload();
    } catch (cause) {
      setPending(false);
      setError(cause instanceof Error ? cause.message : "The support session could not start.");
    }
  };

  return (
    <div className="grid flex-1 place-items-center p-10">
      <div className="w-full max-w-[640px] overflow-hidden rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]">
        <div className="border-b border-border/60 px-7 pt-[26px] pb-[18px]">
          <DetentCloudLogo />
        </div>
        <div className="px-7 pt-6 pb-[26px]">
          <h1 className="mb-2 text-2xl font-semibold tracking-[-0.02em] text-balance">
            Start a support session
          </h1>
          {organization === null ? (
            <p className="text-muted-foreground text-sm" data-testid="support-no-organization">
              Sign in as a support actor. This page starts a support session against the
              organization this hub serves, and the sign-in has to name it.
            </p>
          ) : (
            <>
              <p className="mb-[18px] text-muted-foreground text-sm">
                You are about to act as organization{" "}
                <b className="font-medium text-foreground">{organization}</b>. Everything you do
                is recorded against your own account and the organization can see that the session
                is open, who opened it and when it expires.
              </p>
              {error === null ? null : (
                <div
                  role="alert"
                  className="mb-4 rounded-[10px] border border-destructive/30 bg-destructive/8 px-3.5 py-3 text-[13px] text-destructive-foreground"
                  data-testid="support-error"
                >
                  {error}
                </div>
              )}
              <Button
                size="lg"
                className="w-full justify-center"
                disabled={pending}
                data-testid="support-start"
                onClick={() => void start()}
              >
                {pending ? "Starting the session…" : "Start the support session"}
              </Button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

/**
 * The route, for a reader whose bootstrap did succeed — a staff account that
 * already has a session against this organization and is starting another.
 * The entry point renders `SupportCard` itself where the bootstrap was
 * refused, which is the ordinary case.
 */
export function SupportRoute(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  return (
    <SupportCard
      organization={
        supportOrganization({
          pathname: globalThis.location?.pathname ?? "",
          search: globalThis.location?.search ?? "",
        }) ?? bootstrap?.organization.id ?? null
      }
      csrfToken={bootstrap?.csrf_token ?? null}
    />
  );
}
