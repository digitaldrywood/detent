import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { useAccountBootstrap } from "./context.ts";
import { DetentCloudLogo } from "./Login.tsx";
import { routerBasePath, withoutBasePath } from "../../runtime/basePath.ts";
import { usePageTitle } from "../pageTitle.ts";

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
  if (
    segments[0] === "support" &&
    typeof segments[1] === "string" &&
    segments[1].length > 0
  ) {
    return decodeURIComponent(segments[1]);
  }
  const query = new URLSearchParams(input.search ?? "").get("organization");
  if (query !== null && query.length > 0) return query;
  return organizationFromBody(input.body);
}

/** The CSRF token a refused bootstrap carries for a signed-in account. */
export function supportCSRF(body: unknown): string | null {
  if (body === null || typeof body !== "object") return null;
  const details = (body as Record<string, unknown>)["details"];
  if (details === null || typeof details !== "object") return null;
  const token = (details as Record<string, unknown>)["csrf_token"];
  return typeof token === "string" && token.length > 0 ? token : null;
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
  readonly organization: string | null;
}

export function SupportCard({ organization }: SupportCardProps): React.ReactElement {
  return (
    <div className="grid flex-1 place-items-center p-10">
      <div className="w-full max-w-[640px] overflow-hidden rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]">
        <div className="border-b border-border/60 px-7 pt-[26px] pb-[18px]"><DetentCloudLogo /></div>
        <div className="px-7 pt-6 pb-[26px]">
          <h1 className="mb-2 text-2xl font-semibold tracking-[-0.02em] text-balance">Start a support session</h1>
          {organization === null ? (
            <p className="text-muted-foreground text-sm" data-testid="support-no-organization">
              Sign in as a support actor. The sign-in must name the organization you need to support.
            </p>
          ) : (
            <>
              <p className="mb-[18px] text-muted-foreground text-sm">
                Continue to the secure support request for organization <b className="font-medium text-foreground">{organization}</b>.
                Choose a reason there, then follow the identity provider instructions. Support access remains bound to your account and the selected organization.
              </p>
              <Button size="lg" className="w-full justify-center" data-testid="support-start"
                render={<a href={`/support?organization=${encodeURIComponent(organization)}`} />}>
                Start the support session
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
  usePageTitle("Support");
  const bootstrap = useAccountBootstrap();
  return (
    <SupportCard
      organization={
        supportOrganization({
          pathname: withoutBasePath(globalThis.location?.pathname ?? "", routerBasePath()),
          search: globalThis.location?.search ?? "",
        }) ??
        bootstrap?.organization.id ??
        null
      }
    />
  );
}
