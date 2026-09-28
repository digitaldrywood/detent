// @vitest-environment jsdom
//
// The platform admin prototype: every route renders inside the console
// layout, access follows the staff, support and entitlement roles the entry
// reports, and proposed endpoints are never requested outside preview mode.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import type { ServerResponse } from "node:http";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createPlatformMock, type PlatformRole, type PlatformScenario } from "../../dev/mock-platform.ts";
import { SIGN_IN_PLATFORM, type PlatformOrganization } from "../../src/app/entry/api.ts";
import { makeEntryRouter } from "../../src/app/entry/router.tsx";
import { makePlatformApi } from "../../src/app/platform/api.ts";
import { activePlatformPage, PLATFORM_NAV } from "../../src/app/platform/nav.ts";
import { ALL, filterOrganizations } from "../../src/app/platform/pages/Organizations.tsx";
import { organizationTab } from "../../src/app/platform/pages/OrganizationDetail.tsx";
import { PlatformApiProvider } from "../../src/app/platform/PlatformLayout.tsx";
import { PLATFORM_ROUTE_PATHS } from "../../src/app/platform/routes.tsx";

const assign = vi.fn();

beforeEach(() => {
  assign.mockReset();
  vi.stubGlobal("location", { ...globalThis.location, assign });
  vi.stubGlobal(
    "matchMedia",
    (query: string) =>
      ({
        matches: false,
        media: query,
        addEventListener: () => undefined,
        removeEventListener: () => undefined,
        addListener: () => undefined,
        removeListener: () => undefined,
      }) as unknown as MediaQueryList,
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** A fetch that answers from the mock hub's platform handler, recording every path. */
function mockFetch(options: { scenario?: PlatformScenario; role?: PlatformRole } = {}) {
  const mock = createPlatformMock({ ...options, now: () => Date.parse("2026-09-27T18:00:00Z") });
  const requested: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://mock.local");
    requested.push(`${init?.method ?? "GET"} ${url.pathname}`);
    let status = 404;
    let body = "";
    const response = {
      writeHead: (code: number) => {
        status = code;
        return response;
      },
      end: (chunk?: string) => {
        body = chunk ?? "";
      },
    } as unknown as ServerResponse;
    const handled = await mock.handle({
      response,
      url,
      method: init?.method ?? "GET",
      readBody: async () => (typeof init?.body === "string" ? JSON.parse(init.body) : {}),
    });
    if (!handled) return new Response(JSON.stringify({ code: "not_found", message: "No such endpoint." }), { status: 404 });
    return new Response(body, { status, headers: { "Content-Type": "application/json" } });
  });
  return { fetch, requested };
}

function renderAt(path: string, options: { scenario?: PlatformScenario; role?: PlatformRole; preview?: boolean } = {}) {
  const { fetch, requested } = mockFetch(options);
  const api = makePlatformApi({ fetch, preview: options.preview ?? true });
  const router = makeEntryRouter(createMemoryHistory({ initialEntries: [path] }));
  render(
    <PlatformApiProvider value={api}>
      <RouterProvider router={router as never} />
    </PlatformApiProvider>,
  );
  return { requested };
}

function concrete(path: string): string {
  return path.replace("$organization", "org_harbor").replace("$tab", "members").replace("$subject", "user_staff");
}

const HEADINGS: Record<string, string> = {
  "/platform": "Overview",
  "/platform/organizations": "Organizations",
  "/platform/organizations/$organization": "Harbor Labs",
  "/platform/organizations/$organization/$tab": "Members",
  "/platform/users": "Users",
  "/platform/users/$subject": "staff@detent.example",
  "/platform/runners": "Runners & capacity",
  "/platform/provisioning": "Provisioning & health",
  "/platform/plans": "Plans & grants",
  "/platform/billing": "Billing",
  "/platform/support": "Support sessions",
  "/platform/audit": "Audit log",
  "/platform/settings": "Settings",
};

describe("platform routes", () => {
  it("names a heading for every route", () => {
    expect(Object.keys(HEADINGS).toSorted()).toEqual([...PLATFORM_ROUTE_PATHS].toSorted());
  });

  it.each(PLATFORM_ROUTE_PATHS)("renders %s inside the console", async (path) => {
    renderAt(concrete(path));
    const heading = await screen.findByRole("heading", { level: 1, name: HEADINGS[path] });
    expect(heading).toBeTruthy();
    expect(screen.getByRole("link", { name: "Go to the platform overview" })).toBeTruthy();
  });

  it("lights up the list a detail page belongs to", () => {
    expect(activePlatformPage("/platform")).toBe("overview");
    expect(activePlatformPage("/platform/")).toBe("overview");
    expect(activePlatformPage("/platform/organizations/org_harbor/plan")).toBe("organizations");
    expect(activePlatformPage("/platform/users/user_staff")).toBe("users");
    expect(activePlatformPage("/platform/unknown")).toBe("overview");
  });

  it("links every page from the navigation", () => {
    const navigated = new Set(PLATFORM_NAV.map((item) => item.to));
    const listPages = PLATFORM_ROUTE_PATHS.filter((path) => !path.includes("$"));
    expect([...listPages].toSorted()).toEqual([...navigated].toSorted());
  });

  it("falls back to the overview tab for an unknown tab", () => {
    expect(organizationTab("plan")).toBe("plan");
    expect(organizationTab("nonsense")).toBe("overview");
    expect(organizationTab(undefined)).toBe("overview");
  });
});

describe("platform access", () => {
  it("refuses accounts that are not platform staff", async () => {
    renderAt("/platform", { role: "forbidden" });
    expect(await screen.findByText("Detent staff only")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Go to the platform overview" })).toBeNull();
  });

  it("sends an expired session back to sign-in", async () => {
    renderAt("/platform/organizations", { role: "signed-out" });
    await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM));
  });

  it("gives entitlement administrators the grant controls", async () => {
    renderAt("/platform/organizations/org_harbor/plan", { role: "admin" });
    expect(await screen.findByRole("button", { name: "Grant complimentary plan" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy();
  });

  it("hides grant controls from staff who are not entitlement administrators", async () => {
    renderAt("/platform/organizations/org_harbor/plan", { role: "staff" });
    expect(await screen.findByText("Only entitlement administrators can see grant details and change plans.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Grant complimentary plan" })).toBeNull();
  });

  it("lets only support actors start support access", async () => {
    renderAt("/platform/organizations/org_harbor", { role: "admin" });
    const enabled = await screen.findByRole("button", { name: "Start support access" });
    expect((enabled as HTMLButtonElement).disabled).toBe(false);
    cleanup();
    renderAt("/platform/organizations/org_harbor", { role: "staff" });
    const disabled = await screen.findByRole("button", { name: "Start support access" });
    expect((disabled as HTMLButtonElement).disabled).toBe(true);
    expect(disabled.getAttribute("title")).toBe("Only support actors can start support access.");
  });

  it("lets only support actors end a support session", async () => {
    renderAt("/platform/support", { role: "staff" });
    const end = await screen.findByRole("button", { name: "End session" });
    expect((end as HTMLButtonElement).disabled).toBe(true);
  });

  it("offers suspend only for ready organizations and retry only for failed ones", async () => {
    renderAt("/platform/organizations/org_oakline");
    expect(await screen.findByRole("button", { name: "Retry provisioning" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Suspend" })).toBeNull();
    cleanup();
    renderAt("/platform/organizations/org_kestrel");
    expect(await screen.findByRole("button", { name: "Reactivate" })).toBeTruthy();
  });
});

describe("live and proposed endpoints", () => {
  it("never requests a proposed endpoint outside preview mode", async () => {
    const { requested } = renderAt("/platform", { preview: false });
    expect(await screen.findByText("Not available yet")).toBeTruthy();
    expect(screen.getByText("GET /api/cloud/platform/overview")).toBeTruthy();
    await screen.findByText("Tenant slots");
    expect(requested.toSorted()).toEqual(["GET /api/cloud/platform/health", "GET /api/cloud/platform/organizations"]);
  });

  it("serves the organizations list from the live endpoint without preview", async () => {
    const { requested } = renderAt("/platform/organizations", { preview: false });
    expect(await screen.findByRole("link", { name: "Harbor Labs" })).toBeTruthy();
    expect(requested.every((line) => line === "GET /api/cloud/platform/organizations")).toBe(true);
  });

  it("shows an error with a retry when a read fails", async () => {
    renderAt("/platform/runners", { scenario: "error" });
    expect(await screen.findByText("Detent could not reach the tenant Hubs. Try again shortly.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });

  it("shows empty states when there is nothing to list", async () => {
    renderAt("/platform/organizations", { scenario: "empty" });
    expect(await screen.findByText(/No organizations are registered yet/)).toBeTruthy();
  });
});

describe("organization filters", () => {
  const organization = (id: string, state: string, plan: string | null, creator: string): PlatformOrganization => ({
    id,
    name: id.toUpperCase(),
    state,
    step: "",
    attempts: 0,
    error_code: "",
    managed: true,
    creator_email: creator,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    billing: { available: false },
    can_support: false,
    plan,
    grants: null,
    member_count: null,
    runner_count: null,
  });
  const rows = [
    organization("org_a", "ready", "team", "ana@a.example"),
    organization("org_b", "failed", null, "bo@b.example"),
    organization("org_c", "ready", "pilot_free", "cy@c.example"),
  ];

  it.each([
    { name: "no filter", filter: { query: "", state: ALL, plan: ALL }, ids: ["org_a", "org_b", "org_c"] },
    { name: "state", filter: { query: "", state: "ready", plan: ALL }, ids: ["org_a", "org_c"] },
    { name: "plan", filter: { query: "", state: ALL, plan: "team" }, ids: ["org_a"] },
    { name: "creator search", filter: { query: "BO@", state: ALL, plan: ALL }, ids: ["org_b"] },
    { name: "id search", filter: { query: "org_c", state: "ready", plan: ALL }, ids: ["org_c"] },
    { name: "no match", filter: { query: "zzz", state: ALL, plan: ALL }, ids: [] },
  ])("filters by $name", ({ filter, ids }) => {
    expect(filterOrganizations(rows, filter).map((row) => row.id)).toEqual(ids);
  });
});
