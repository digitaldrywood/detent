// @vitest-environment jsdom
//
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import { STALE_STAFF_MESSAGE } from "../../src/app/entry/PlatformStaff.tsx";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import { type EntryApi, type PlatformAudit, type PlatformMembers, makeEntryApi, SIGN_IN_PLATFORM } from "../../src/app/entry/api.ts";
import {
  EntryApiContext,
} from "../../src/app/entry/EntryScreens.tsx";
import { auditAction, auditActor } from "../../src/app/entry/platformAuditPresentation.ts";
import { formatBytes } from "../../src/app/entry/PlatformConsole.tsx";
import { makeEntryRouter } from "../../src/app/entry/router.tsx";
import { STALE_PLAN_MESSAGE } from "../../src/app/entry/ComplimentaryPlans.tsx";
import { tenantNeedsAttention, tenantRow } from "../../src/app/entry/tenantList.ts";
import type { PlatformOrganization } from "../../src/app/entry/api.ts";
import userResults from "../../../../tests/visual/platform-users-data.json";

const assign = vi.fn();

beforeEach(() => {
  assign.mockReset();
  vi.stubGlobal("location", { assign, search: "", pathname: "/" });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const organizations = {
  email: "admin@example.com",
  csrf: "staff-csrf",
  can_support: true,
  unavailable: ["plan", "grants", "member_count", "runner_count"],
  organizations: [
    {
      id: "org_alpha", name: "Alpha", state: "ready", step: "publish", attempts: 0, error_code: "", managed: true,
      creator_email: "member@example.test", created_at: "2026-09-20T10:00:00Z", updated_at: "2026-09-20T10:05:00Z",
      billing: { available: true, status: "active" }, can_support: true, plan: null, grants: null, member_count: null, runner_count: null,
    },
    {
      id: "org_beta", name: "Beta", state: "failed", step: "tenant_files", attempts: 1, error_code: "tenant_start_failed", error_detail: "the tenant Hub exited 5 times in a row without staying up (last: exit status 1)", managed: true,
      creator_email: "eve@example.test", created_at: "2026-09-21T10:00:00Z", updated_at: "2026-09-21T10:05:00Z",
      billing: { available: false }, can_support: false, plan: null, grants: null, member_count: null, runner_count: null,
    },
  ],
};

const allowlist = {
  self_service: true,
  open: false,
  allowed_emails: ["member@example.test"],
  allowed_domains: ["example.org"],
  source: { file: "/etc/detent/cloud.yaml", keys: ["allocation.allowed_emails", "allocation.allowed_domains"] },
};

const health = {
  registry: { ok: true },
  tenants: { expected: 2, running: 1 },
  tenant_hubs: [
    { id: "org_alpha", name: "Alpha", running: true, uptime_seconds: 3600, restarts_24h: 0, database_bytes: 1024 },
    { id: "org_beta", name: "Beta", running: false, uptime_seconds: null, restarts_24h: 2, database_bytes: null },
  ],
  admission: {
    tenants: 2, max_tenants: 20, allocating: 0, max_concurrent: 1,
    disk_measured: true, free_disk_bytes: 10 * 1024 ** 3, min_free_disk_bytes: 2 * 1024 ** 3,
    memory_measured: false, available_memory_bytes: 0, min_available_memory_bytes: 512 * 1024 ** 2,
  },
};

const entitlements = {
  organization_id: "org_alpha",
  base: { id: "pilot_free", version: 1 },
  effective_base: { id: "pilot_free", version: 1 },
  source: "base",
  revision: 3,
  grants: [
    {
      id: "comp_existing", plan: { id: "comp_team", version: 1 }, scope: ["hosted_artifacts", "projects"],
      starts_at: "2026-09-20T10:00:00Z", expires_at: "2026-12-31T23:59:59Z", reason: "design partner",
      granted_by: "plans@example.com", granted_at: "2026-09-20T10:00:00Z",
    },
  ],
  plans: [
    { id: "pilot_free", version: 1, features: ["collaboration"], allowances: { projects: 10 } },
    { id: "comp_team", version: 1, features: ["collaboration", "hosted_artifacts"], allowances: { projects: 20 } },
  ],
};

const administrator = { ...organizations, can_grant: true };

function fakeApi(overrides: Partial<EntryApi> = {}): EntryApi {
  const api: EntryApi = {
    organizations: vi.fn(async () => ({ email: "admin@example.com", csrf: "staff-csrf", organizations: [], pending: [], can_create: false, platform_role: "admin" })),
    session: vi.fn(async () => ({ email: "admin@example.com", csrf: "staff-csrf", can_create: false, platform_role: "admin" })),
    provisioning: vi.fn(async () => ({ id: "", name: "", state: "", step: "", error: "", can_resume: false })),
    createOrganization: vi.fn(async () => ({ next: "/" })),
    resume: vi.fn(async () => ({ next: "/" })),
    platformTenant: vi.fn(async (id: string) => ({
      organization: organizations.organizations.find((item) => item.id === id)!,
      csrf: "staff-csrf", can_grant: true, can_resume: id === "org_beta", unavailable: [],
      credit_balance_micros: null,
      members: [{ id: "owner", email: "member@example.test", role: "owner" }],
      runners: [{ id: "runner_one", name: "Mac Studio", health: "online" }],
      projects: [{ id: "prj_one", name: "Detent" }],
      events: [{ event: "requested", generation: 1, recorded_at: "2026-09-20T10:00:00Z" }],
      entitlements: await api.platformEntitlements(id),
    })),
    adjustPlatformAICredits: vi.fn(async () => ({ organization_id: "org_alpha", balance_micros: 0 })),
    resumePlatformTenant: vi.fn(async () => ({ next: "/platform/tenants" })),
    platformUsers: vi.fn(async () => ({ users: [], unsearched: [] })),
    platformOrganizations: vi.fn(async () => organizations),
    platformAllowlist: vi.fn(async () => allowlist),
    platformMembers: vi.fn(async () => ({ members: [], revision: 1, self: { email: "admin@example.com", role: "admin" } })),
    changePlatformMember: vi.fn(async () => ({ email: "new@example.test", role: "viewer", revision: 2 })),
    platformAudit: vi.fn(async () => ({ rows: [], events: [], tenants: [], next_cursor: "" })),
    platformHealth: vi.fn(async () => health),
    platformEntitlements: vi.fn(async () => entitlements),
    changePlatformEntitlement: vi.fn(async () => ({ action: "grant", grant_id: "comp_new" })),
    ...overrides,
  };
  return api;
}

function renderWith(api: EntryApi, element: React.ReactElement) {
  return render(<EntryApiContext.Provider value={api}>{element}</EntryApiContext.Provider>);
}

async function renderPlatform(api: EntryApi, path = "/platform/tenants") {
  const router = makeEntryRouter(createMemoryHistory({ initialEntries: [path] }));
  await act(async () => {
    renderWith(api, <RouterProvider router={router} />);
    await router.load();
  });
  return router;
}

const credits = {
  balance_micros: 100_000_000, spent_micros: 750_000, month: "2026-10", auto_enabled: false,
  history: [
    { amount_micros: 100_000_000, kind: "complimentary", actor: "staff@example.test", reason: "Pilot credits", at: "2026-10-09T12:00:00Z" },
    { amount_micros: 5_000_000, kind: "purchase", actor: "", reason: "", at: "2026-10-08T12:00:00Z" },
  ],
};

function creditApi(overrides: Partial<EntryApi> = {}): EntryApi {
  const api = fakeApi(overrides);
  const read = api.platformTenant;
  api.platformTenant = vi.fn(async (id) => ({ ...await read(id), credit_balance_micros: credits.balance_micros, ai_credits: credits }));
  return api;
}

describe("platform console", () => {
  it("debounces URL-backed user search and rejects short queries", async () => {
    const search = vi.fn(async () => userResults);
    const router = await renderPlatform(fakeApi({ platformUsers: search }), "/platform/users");
    const input = await screen.findByRole("searchbox", { name: "Search by email" });
    const navigate = vi.spyOn(router, "navigate");
    let allowNavigation!: (blocked: boolean) => void;
    const pendingNavigation = new Promise<boolean>((resolve) => { allowNavigation = resolve; });
    const unblock = router.history.block({ blockerFn: () => pendingNavigation });
    vi.useFakeTimers();
    fireEvent.change(input, { target: { value: "ab" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(299); });
    expect(navigate).not.toHaveBeenCalled();
    expect(search).not.toHaveBeenCalled();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(navigate).toHaveBeenCalledTimes(1);
    expect(router.state.location.search).toEqual({});
    await act(async () => { allowNavigation(false); });
    await act(async () => { await navigate.mock.results[0]!.value; });
    unblock();
    expect(search).not.toHaveBeenCalled();
    expect(router.state.location.search).toEqual({ q: "ab" });
    fireEvent.change(input, { target: { value: "example" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    fireEvent.change(input, { target: { value: " example.test " } });
    await act(async () => { await vi.advanceTimersByTimeAsync(299); });
    expect(search).not.toHaveBeenCalled();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(navigate).toHaveBeenCalledTimes(2);
    await act(async () => { await navigate.mock.results[1]!.value; });
    vi.useRealTimers();
    expect(search).toHaveBeenCalledExactlyOnceWith("example.test");
    expect(router.state.location.search).toEqual({ q: "example.test" });
    const table = await screen.findByRole("table", { name: "Users" });
    const member = within(table).getByRole("row", { name: /sam@example.test/ });
    expect(member.textContent).toContain("support");
    expect(member.textContent).toContain("2026-10-06 14:02Z");
    expect(within(member).getByText("2")).toBeTruthy();
    expect(within(table).getByRole("row", { name: /alex/ }).textContent).toContain("None");
    expect(screen.getByText("Not searched: Slow Tenant")).toBeTruthy();
  });

  it("restores search and selected user on back and reload with explicit empty sections", async () => {
    const api = fakeApi({ platformUsers: vi.fn(async () => userResults) });
    const router = await renderPlatform(api, "/platform/users?q=example.test");
    const input = await screen.findByRole("searchbox") as HTMLInputElement;
    expect(input.value).toBe("example.test");
    fireEvent.click(await screen.findByRole("link", { name: "alex+test@example.test" }));
    const account = await screen.findByRole("region", { name: "Account" });
    expect(account.textContent).toContain("user_01HX_alex");
    expect(screen.getByRole("region", { name: "Organizations" }).textContent).toContain("No organization membership");
    expect(screen.getByRole("region", { name: "Pending invitations" }).textContent).toContain("No pending invitations");
    expect(screen.queryByRole("table")).toBeNull();
    expect(api.session).toHaveBeenCalledTimes(1);
    const selected = router.state.location.href;
    expect(router.state.location.search).toEqual({ q: "example.test" });
    await act(async () => { router.history.back(); });
    expect((await screen.findByRole("searchbox") as HTMLInputElement).value).toBe("example.test");
    cleanup();
    await renderPlatform(api, selected);
    expect((await screen.findByRole("region", { name: "Account" })).textContent).toContain("alex+test@example.test");
    expect(api.platformUsers).toHaveBeenLastCalledWith("alex+test@example.test");
    fireEvent.click(within(screen.getByRole("heading", { level: 1 })).getByRole("link", { name: "Users" }));
    expect((await screen.findByRole("searchbox") as HTMLInputElement).value).toBe("example.test");
  });

  it("separates user organizations and pending invitations with client-side tenant links", async () => {
    const original = userResults.users[0]!;
    const invitation = original.invitations[0]!;
    let invitationsResult = [invitation, { ...invitation, expires_at: "2026-10-14T00:00:00Z" }, invitation];
    const errors = vi.spyOn(console, "error");
    const api = fakeApi({ platformUsers: vi.fn(async () => ({ ...userResults, users: [{ ...original, invitations: invitationsResult }] })) });
    const router = await renderPlatform(api, "/platform/users/sam%40example.test");
    const account = await screen.findByRole("region", { name: "Account" });
    expect(account.textContent).toContain("user_01HX_sam");
    expect(within(account).getByRole("link", { name: "Manage in Staff" }).getAttribute("href")).toBe("/platform/staff");
    const memberships = screen.getByRole("region", { name: "Organizations" });
    expect(memberships.textContent).toContain("Joined 2026-09-28");
    expect(memberships.textContent).toContain("owner");
    expect(within(memberships).getByRole("link", { name: "Example Alpha" }).getAttribute("href")).toBe("/platform/tenants/org_alpha");
    const invitations = screen.getByRole("region", { name: "Pending invitations" });
    expect(invitations.textContent).toContain("Expires 2026-10-13");
    expect(within(invitations).getAllByRole("link", { name: "Example Co" })).toHaveLength(3);
    expect(within(invitations).getAllByRole("link", { name: "Example Co" })[0]!.getAttribute("href")).toBe("/platform/tenants/org_example");
    expect(invitations.textContent).toContain("Expires 2026-10-14");
    invitationsResult = [invitationsResult[1]!];
    await act(async () => { await router.navigate({ to: "/platform/users/$email", params: { email: "SAM@example.test" } } as never); });
    await waitFor(() => {
      expect(within(invitations).getAllByRole("link", { name: "Example Co" })).toHaveLength(1);
      expect(invitations.textContent).toContain("Expires 2026-10-14");
      expect(invitations.textContent).not.toContain("Expires 2026-10-13");
    });
    expect(errors.mock.calls.filter((args) => args.some((arg) => typeof arg === "string" && arg.includes("same key")))).toEqual([]);
    errors.mockRestore();
  });

  it("preserves the 50-result notice and explicit empty search row", async () => {
    const api = fakeApi({ platformUsers: vi.fn(async (query: string) => ({
      ...userResults,
      users: query === "missing" ? [] : Array.from({ length: 50 }, (_, index) => ({ ...userResults.users[0]!, email: `user${index}@example.test` })),
    })) });
    await renderPlatform(api, "/platform/users?q=example.test");
    expect(await screen.findByText("Showing up to 50 users. Refine your search to find other users.")).toBeTruthy();
    const input = screen.getByRole("searchbox");
    fireEvent.change(input, { target: { value: "missing" } });
    fireEvent.submit(input.closest("form")!);
    expect(await screen.findByText("No users match")).toBeTruthy();
    expect(screen.getByRole("row", { name: "No users match" })).toBeTruthy();
    expect(screen.getByText("Not searched: Slow Tenant")).toBeTruthy();
  });

  it("keeps user pages staff-only and hides Staff management from non-admin staff", async () => {
    const search = vi.fn(async () => userResults);
    await renderPlatform(fakeApi({ platformUsers: search, session: vi.fn(async () => ({ email: "person@example.test", csrf: "c", platform_role: "" })) }), "/platform/users/sam%40example.test");
    expect(await screen.findByText("You don't have access to this page")).toBeTruthy();
    expect(search).not.toHaveBeenCalled();
    cleanup();
    await renderPlatform(fakeApi({ platformUsers: search, session: vi.fn(async () => ({ email: "support@example.test", csrf: "c", platform_role: "support" })) }), "/platform/users/sam%40example.test");
    await screen.findByRole("region", { name: "Account" });
    expect(screen.queryByRole("link", { name: "Manage in Staff" })).toBeNull();
  });

  it.each([401, 403, 503])("handles user search failure %s", async (status) => {
    let refuse!: (error: AccountError) => void;
    const search = vi.fn().mockImplementationOnce(() => new Promise<typeof userResults>((_, reject) => { refuse = reject; })).mockResolvedValue(userResults);
    const api = fakeApi({ platformUsers: search });
    const user = userEvent.setup();
    const router = await renderPlatform(api, "/platform/users");
    const input = await screen.findByRole("searchbox", { name: "Search by email" });
    await user.type(input, "member");
    await waitFor(() => expect(api.platformUsers).toHaveBeenCalledExactlyOnceWith("member"));
    expect(screen.getByRole("status", { name: "Searching users" })).toBeTruthy();
    await act(async () => { refuse(new AccountError({ status, code: "refused", message: "Search refused" })); });
    expect((await screen.findByRole("alert")).textContent).toContain("Search refused");
    expect(screen.queryByText("No users match")).toBeNull();
    if (status === 401) await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM));
    expect((input as HTMLInputElement).disabled).toBe(false);
    fireEvent.change(input, { target: { value: " member " } });
    fireEvent.submit(input.closest("form")!);
    await screen.findByRole("table", { name: "Users" });
    expect(search).toHaveBeenCalledTimes(2);
    expect(search).toHaveBeenLastCalledWith("member");
    expect(router.state.location.search).toEqual({ q: "member" });
    expect(screen.queryByRole("alert")).toBeNull();
  });
  it.each(["admin", "support", "billing", "viewer"])("shows the shell and readable navigation for %s", async (role) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "member@example.test", csrf: "c", platform_role: role })) });
    await renderPlatform(api);
    const nav = await screen.findByRole("navigation", { name: "Platform navigation" });
    await screen.findByRole("heading", { name: "Tenants", level: 1 });
    expect(within(nav).getAllByRole("link").map((link) => link.getAttribute("aria-label"))).toEqual(
      role === "admin" ? ["Tenants", "Users", "Audit log", "Health", "Sign-up allowlist", "Staff"] : ["Tenants", "Users", "Audit log", "Health", "Sign-up allowlist"],
    );
    expect(within(nav).getByRole("link", { name: "Tenants" }).getAttribute("data-active")).toBe("true");
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(document.querySelector('[data-slot="sidebar-footer"]')?.textContent).toContain(role[0]!.toUpperCase() + role.slice(1));
    expect(screen.queryByRole("button", { name: "Open organization" })).toBeNull();
    expect(api.platformHealth).toHaveBeenCalled();
    expect(api.platformAllowlist).not.toHaveBeenCalled();
  });

  it("opens the account's organizations and preserves the header when navigating", async () => {
    const api = fakeApi({ organizations: vi.fn(async () => ({
      email: "admin@example.com", csrf: "c", platform_role: "admin",
      organizations: [
        { id: "org_alpha", name: "My Alpha", url: "/organizations/org_alpha/organization" },
        { id: "org_beta", name: "My Beta", url: "/organizations/org_beta/work" },
      ],
    })) });
    const router = await renderPlatform(api);
    const nav = await screen.findByRole("navigation", { name: "Your organizations" });
    expect(within(nav).getByRole("link", { name: "My Alpha" }).getAttribute("href")).toBe("/organizations/org_alpha/organization");
    expect(within(nav).getByRole("link", { name: "My Beta" }).getAttribute("href")).toBe("/organizations/org_beta/work");
    fireEvent.click(screen.getByRole("link", { name: "Health" }));
    await screen.findByRole("alert", { name: "Service health" });
    expect(router.state.location.pathname).toBe("/platform/health");
    expect(document.title).toBe("Health · Platform · Detent");
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(api.session).toHaveBeenCalledTimes(1);
    expect(api.organizations).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("table", { name: "Tenants" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Complimentary plans" })).toBeNull();
  });

  it.each(["support", "billing", "viewer"])("refuses a direct Staff route for %s inside the shell", async (role) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "staff@example.test", csrf: "c", platform_role: role })) });
    await renderPlatform(api, "/platform/staff");
    expect(await screen.findByText("You don't have access to this page")).toBeTruthy();
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Staff" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add staff member" })).toBeNull();
    expect(api.platformMembers).not.toHaveBeenCalled();
  });

  it("lists newest tenants first with human status, owner and explicit unavailable values", async () => {
    await renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    await within(table).findByRole("link", { name: "Alpha" });
    const rows = within(table).getAllByRole("row");
    expect(within(rows[1]!).getByText("Beta")).toBeTruthy();
    expect(within(rows[1]!).getByText("Failed")).toBeTruthy();
    expect(within(rows[1]!).getByText("Tenant files")).toBeTruthy();
    expect(within(rows[2]!).getByText("member@example.test")).toBeTruthy();
    expect(within(rows[2]!).getByText("Ready")).toBeTruthy();
    expect(within(rows[2]!).getByText("2026-09-20")).toBeTruthy();
    expect(within(table).getAllByText("Unavailable").length).toBeGreaterThan(0);
    expect(table.textContent).not.toContain("tenant_start_failed");
    expect(table.textContent).not.toContain("tenant_files");
    expect(table.textContent).not.toContain("active");
    expect(screen.getByText(/Not shown: plan, grants, member count, runner count/)).toBeTruthy();
  });

  it("round-trips tenant filters and sorting through URLs and Back from the tenant page", async () => {
    const user = userEvent.setup();
    const data = { ...organizations, unavailable: [], organizations: organizations.organizations.map((tenant) => ({ ...tenant, plan: "starter", grants: 1 })) };
    const api = fakeApi({ platformOrganizations: vi.fn(async () => data) });
    const router = await renderPlatform(api, "/platform/tenants?q=member&segment=complimentary&plan=starter&status=Ready&sort=name&direction=desc");
    const table = await screen.findByRole("table", { name: "Tenants" });
    const link = await within(table).findByRole("link", { name: "Alpha" });
    expect((screen.getByRole("textbox", { name: "Search tenants" }) as HTMLInputElement).value).toBe("member");
    expect(screen.getByRole("combobox", { name: "Plan" }).textContent).toContain("Starter");
    expect(screen.getByRole("combobox", { name: "Status" }).textContent).toContain("Ready");
    expect(within(table).getByText("Until 2026-12-31")).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Complimentary/ }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("columnheader", { name: "Tenant" }).getAttribute("aria-sort")).toBe("descending");
    fireEvent.click(link);
    await screen.findByRole("heading", { name: "Alpha" });
    expect(router.state.location.pathname).toBe("/platform/tenants/org_alpha");
    expect(router.state.location.search).toMatchObject({ q: "member", segment: "complimentary", plan: "starter", status: "Ready", sort: "name", direction: "desc" });
    fireEvent.click(screen.getByRole("button", { name: "Members 1" }));
    await screen.findByRole("region", { name: "Members" });
    expect(router.state.location.search).toMatchObject({ q: "member", segment: "complimentary", plan: "starter", status: "Ready", sort: "name", direction: "desc", section: "members" });
    fireEvent.click(within(screen.getByRole("main")).getByRole("link", { name: /^Tenants$/ }));
    await within(await screen.findByRole("table", { name: "Tenants" })).findByRole("link", { name: "Alpha" });
    await user.click(screen.getByRole("combobox", { name: "Plan" }));
    await user.click(await screen.findByRole("option", { name: "All plans" }));
    await waitFor(() => expect(router.state.location.search.plan).toBeUndefined());
    await user.click(screen.getByRole("combobox", { name: "Status" }));
    await user.click(await screen.findByRole("option", { name: "All statuses" }));
    await waitFor(() => expect(router.state.location.search.status).toBeUndefined());
    await user.click(screen.getByRole("button", { name: /^Needs attention/ }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "org_beta" } });
    await user.click(screen.getByRole("button", { name: "Created" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ q: "org_beta", segment: "attention", sort: "created", direction: "desc" }));
    const url = router.state.location.href;
    cleanup();
    await renderPlatform(api, url);
    const reloaded = await screen.findByRole("table", { name: "Tenants" });
    expect(await within(reloaded).findByRole("link", { name: "Beta" })).toBeTruthy();
    expect(within(reloaded).queryByRole("link", { name: "Alpha" })).toBeNull();
    expect((screen.getByRole("textbox", { name: "Search tenants" }) as HTMLInputElement).value).toBe("org_beta");
  });

  it.each([
    ["failed setup", "failed", null, null, null, true],
    ["every runner offline", "ready", 2, 0, 10, true],
    ["some runners online", "ready", 2, 1, 10, false],
    ["no runners configured", "ready", 0, 0, null, false],
    ["unknown runner health", "ready", 2, null, null, false],
    ["zero credits", "ready", 0, 0, 0, true],
    ["negative credits", "ready", 1, 1, -1, true],
    ["unavailable credits", "ready", null, null, null, false],
  ] as const)("needs attention: %s", (_name, state, runners, onlineRunners, creditBalance, expected) => {
    const organization: PlatformOrganization = { ...organizations.organizations[0]!, state, runner_count: runners };
    expect(tenantNeedsAttention({ organization, onlineRunners, creditBalance, grantExpiry: null })).toBe(expected);
  });

  it("filters tenants with all runners offline and keeps unreadable tenants visible", async () => {
    const api = fakeApi({
      platformOrganizations: vi.fn(async () => ({ ...organizations, organizations: organizations.organizations.map((tenant) => ({ ...tenant, runner_count: 2 })) })),
    });
    const original = api.platformTenant;
    api.platformTenant = vi.fn(async (id) => ({ ...await original(id), runners: [{ id: "one", name: "One", health: "offline" }, { id: "two", name: "Two", health: "offline" }] }));
    await renderPlatform(api, "/platform/tenants?segment=attention");
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(await within(table).findByRole("link", { name: "Alpha" })).toBeTruthy();
    expect(within(table).getByRole("link", { name: "Beta" })).toBeTruthy();
    expect(within(table).getByText("0 online")).toBeTruthy();
    cleanup();
    api.platformTenant = vi.fn(async () => { throw new AccountError({ status: 503, code: "unavailable", message: "Unavailable" }); });
    await renderPlatform(api);
    const incomplete = await screen.findByRole("table", { name: "Tenants" });
    expect(await within(incomplete).findByRole("link", { name: "Alpha" })).toBeTruthy();
    expect(screen.getByText(/Not shown: runner health/)).toBeTruthy();
  });

  it("keeps loading, failure, retry and empty states in the tenant list template", async () => {
    let reject: ((error: AccountError) => void) | undefined;
    const pending = new Promise<typeof organizations>((_resolve, fail) => { reject = fail; });
    const read = vi.fn(() => pending);
    await renderPlatform(fakeApi({ platformOrganizations: read }));
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(table.getAttribute("aria-busy")).toBe("true");
    expect(screen.queryByText("No tenants yet")).toBeNull();
    reject!(new AccountError({ status: 503, code: "registry_unavailable", message: "Registry unavailable" }));
    expect((await screen.findByRole("alert")).textContent).toContain("Tenants could not be loaded");
    expect(screen.queryByRole("table", { name: "Tenants" })).toBeNull();
    read.mockImplementation(async () => ({ ...organizations, organizations: [] }));
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await within(screen.getByRole("region", { name: "Tenant list" })).findByText("No tenants yet")).toBeTruthy();
  });

  it("retains grant expiry and recognizes connected runners outside working hours", () => {
    const organization = organizations.organizations[0]!;
    const detail = { organization, entitlements, runners: [], members: [], projects: [], events: [], unavailable: [], csrf: "c", can_grant: false, can_resume: false, credit_balance_micros: null };
    expect(tenantRow(organization, detail).grantExpiry).toBe("2026-12-31T23:59:59Z");
    expect(tenantRow(organization, { ...detail, entitlements: { ...entitlements, grants: [{ ...entitlements.grants[0]!, expires_at: null }] } }).grantExpiry).toBeNull();
    const scheduled = tenantRow({ ...organization, runner_count: 2 }, {
      ...detail,
      runners: [{ id: "scheduled", name: "Scheduled", health: "outside_hours" }, { id: "offline", name: "Offline", health: "offline" }],
    });
    expect(scheduled.onlineRunners).toBe(1);
    expect(tenantNeedsAttention(scheduled)).toBe(false);
  });

  it.each([
    [10_250_000, 10.25, false],
    [1, 0.000001, false],
    [0, 0, true],
    [-2_500_000, -2.5, true],
    [null, null, false],
  ] as const)("maps credit balance %s micros to dollars", (micros, dollars, attention) => {
    const organization = { ...organizations.organizations[0]!, runner_count: 0 };
    const detail = { organization, entitlements: null, runners: [], members: [], projects: [], events: [], unavailable: [], csrf: "c", can_grant: false, can_resume: false, credit_balance_micros: micros };
    const row = tenantRow(organization, detail);
    expect(row.creditBalance).toBe(dollars);
    expect(tenantNeedsAttention(row)).toBe(attention);
    expect(tenantRow(organization).creditBalance).toBeNull();
  });

  it("shows dollar balances and filters exhausted credits using the loaded tenant detail", async () => {
    const api = fakeApi({
      platformOrganizations: vi.fn(async () => ({ ...organizations, organizations: organizations.organizations.map((organization) => ({ ...organization, state: "ready", runner_count: 0, grants: 0 })) })),
    });
    const original = api.platformTenant;
    api.platformTenant = vi.fn(async (id) => ({ ...await original(id), credit_balance_micros: id === "org_alpha" ? 10_250_000 : 0 }));
    await renderPlatform(api);
    expect(await screen.findByText("$10.25")).toBeTruthy();
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(within(table).getByText("$10.25")).toBeTruthy();
    expect(await within(table).findByText("$0.00")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Needs attention/ }));
    await waitFor(() => expect(within(table).queryByRole("link", { name: "Alpha" })).toBeNull());
    expect(within(table).getByRole("link", { name: "Beta" })).toBeTruthy();
    expect(api.platformTenant).toHaveBeenCalledTimes(2);
  });

  it("requires support confirmation and submits the reason, note and CSRF token", async () => {
    const user = userEvent.setup();
    await renderPlatform(fakeApi(), "/platform/tenants/org_alpha");
    const start = await screen.findByRole("button", { name: "Support access" });
    expect(screen.queryByRole("button", { name: "Start support access" })).toBeNull();
    fireEvent.click(start);
    let dialog = await screen.findByRole("dialog", { name: "Start support access" });
    const submit = vi.fn((event: Event) => event.preventDefault());
    const form = within(dialog).getByRole("button", { name: "Start support access" }).closest("form")!;
    form.addEventListener("submit", submit);
    expect(form.getAttribute("method")).toBe("post");
    expect(form.getAttribute("action")).toBe("/support/start");
    expect(submit).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(start);
    dialog = await screen.findByRole("dialog", { name: "Start support access" });
    await user.click(within(dialog).getByRole("combobox", { name: "Reason" }));
    expect((await screen.findAllByRole("option")).map((option) => option.textContent)).toEqual(["Customer asked for help", "Account recovery", "Troubleshooting"]);
    expect(dialog.textContent).not.toMatch(/customer-request|account-recovery/);
    await user.click(await screen.findByRole("option", { name: "Account recovery" }));
    expect(within(dialog).getByRole("combobox", { name: "Reason" }).textContent).toBe("Account recovery");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Note (optional)" }), { target: { value: "Ticket 123" } });
    const confirmed = within(dialog).getByRole("button", { name: "Start support access" }).closest("form")!;
    confirmed.addEventListener("submit", submit);
    fireEvent.click(within(dialog).getByRole("button", { name: "Start support access" }));
    expect(submit).toHaveBeenCalledTimes(1);
    expect(Object.fromEntries(new FormData(confirmed))).toEqual({ organization: "org_alpha", csrf: "staff-csrf", reason: "account-recovery", note: "Ticket 123" });
  });

  it("shows restricted access with individual rows and filters both allowlists live", async () => {
    const api = fakeApi({ platformAllowlist: vi.fn(async () => ({ ...allowlist,
      allowed_domains: ["example.org", "example.com"],
      allowed_emails: ["member@example.test", "staff@example.com", "operator@example.test"],
    })) });
    await renderPlatform(api, "/platform/allowlist");
    const panel = await screen.findByRole("region", { name: "Sign-up allowlist" });
    const access = within(panel).getByRole("region", { name: "Access" });
    expect(within(access).getByRole("heading", { name: "Who can sign up" })).toBeTruthy();
    expect(within(access).getByText("Restricted")).toBeTruthy();
    expect(within(access).getByText("Only the domains and emails below. Changes are made in the Cloud configuration.")).toBeTruthy();
    expect(panel.textContent).not.toContain(allowlist.source.file);
    for (const key of allowlist.source.keys) expect(panel.textContent).not.toContain(key);
    const domains = within(panel).getByRole("region", { name: "Domains" });
    const emails = within(panel).getByRole("region", { name: "Individual emails" });
    expect(within(domains).getByText("2")).toBeTruthy();
    expect(within(emails).getByText("3")).toBeTruthy();
    expect(within(domains).getAllByRole("row").map((row) => row.textContent)).toEqual(["example.org", "example.com"]);
    expect(within(emails).getAllByRole("row").map((row) => row.textContent)).toEqual(["member@example.test", "staff@example.com", "operator@example.test"]);
    const filter = within(panel).getByRole("searchbox", { name: "Filter domains and emails" });
    fireEvent.change(filter, { target: { value: " COM " } });
    expect(within(domains).getAllByRole("row").map((row) => row.textContent)).toEqual(["example.com"]);
    expect(within(emails).getAllByRole("row").map((row) => row.textContent)).toEqual(["staff@example.com"]);
    expect(within(domains).getByText("1")).toBeTruthy();
    expect(within(emails).getByText("1")).toBeTruthy();
    fireEvent.change(filter, { target: { value: "no-match" } });
    expect(within(domains).getByText("No domains match this filter.")).toBeTruthy();
    expect(within(emails).getByText("No individual emails match this filter.")).toBeTruthy();
    expect(within(domains).getByText("0")).toBeTruthy();
    expect(within(emails).getByText("0")).toBeTruthy();
    fireEvent.change(filter, { target: { value: "" } });
    expect(within(domains).getAllByRole("row")).toHaveLength(2);
    expect(within(emails).getAllByRole("row")).toHaveLength(3);
    expect(api.platformAllowlist).toHaveBeenCalledTimes(1);
    expect(within(panel).queryByRole("button")).toBeNull();
  });

  it.each([
    { self_service: true, open: true, mode: "Open", description: "Any verified account may create an organization." },
    { self_service: true, open: undefined, mode: "Open", description: "Any verified account may create an organization." },
    { self_service: false, open: undefined, mode: "Disabled", description: "Self-service organization creation is not configured." },
  ])("shows $mode access without list sections for $self_service self-service and $open open mode", async ({ self_service, open, mode, description }) => {
    await renderPlatform(fakeApi({ platformAllowlist: vi.fn(async () => ({ ...allowlist, self_service, open,
      allowed_domains: [], allowed_emails: [],
    })) }), "/platform/allowlist");
    const panel = await screen.findByRole("region", { name: "Sign-up allowlist" });
    expect(within(panel).getByText(mode)).toBeTruthy();
    expect(within(panel).getByText(`${description} Changes are made in the Cloud configuration.`)).toBeTruthy();
    expect(within(panel).queryByRole("region", { name: "Domains" })).toBeNull();
    expect(within(panel).queryByRole("region", { name: "Individual emails" })).toBeNull();
    expect(within(panel).queryByRole("searchbox")).toBeNull();
  });

  it.each(["allowed_domains", "allowed_emails"] as const)("keeps restricted access when only %s is configured", async (list) => {
    await renderPlatform(fakeApi({ platformAllowlist: vi.fn(async () => ({ ...allowlist, open: undefined,
      allowed_domains: [], allowed_emails: [], [list]: allowlist[list],
    })) }), "/platform/allowlist");
    const panel = await screen.findByRole("region", { name: "Sign-up allowlist" });
    expect(within(panel).getByText("Restricted")).toBeTruthy();
    expect(within(panel).getByText(allowlist[list][0]!)).toBeTruthy();
    expect(within(panel).getByText(list === "allowed_domains" ? "No individual emails configured." : "No domains configured.")).toBeTruthy();
  });

  it("shows registry, tenant and admission health", async () => {
    await renderPlatform(fakeApi(), "/platform/health");
    const panel = await screen.findByRole("alert", { name: "Service health" });
    expect(panel.getAttribute("data-variant")).toBe("error");
    expect(panel.textContent).toContain("Registry reachable. 1 of 2 tenant Hubs running.");
    const capacity = screen.getByRole("region", { name: "Capacity" });
    expect(within(capacity).getByText("2 / 20")).toBeTruthy();
    expect(within(capacity).getByRole("meter", { name: "Free disk" }).getAttribute("aria-valuetext")).toBe("10.0 GiB");
    expect(within(capacity).getByText("Floor 2.0 GiB")).toBeTruthy();
    expect(within(capacity).getByText("Not measurable")).toBeTruthy();
    const table = screen.getByRole("table", { name: "Tenant Hubs" });
    expect(within(table).getByRole("link", { name: "Beta" }).getAttribute("href")).toBe("/platform/tenants?tenant=org_beta");
    expect(within(table).getByText("Down")).toBeTruthy();
    expect(within(table).getByText("1 hours")).toBeTruthy();
  });

  it.each([
    ["Free disk", 121, "Above floor"],
    ["Free disk", 120, "Near limit"],
    ["Free disk", 100, "At limit"],
    ["Free disk", 99, "At limit"],
    ["Available memory", 120, "Near limit"],
    ["Available memory", 100, "At limit"],
    ["Tenant slots", 80, "Near limit"],
    ["Tenant slots", 100, "At limit"],
  ])("labels %s at %s as %s", async (label, reading, expected) => {
    const admission = { ...health.admission, memory_measured: true, available_memory_bytes: 1000, min_available_memory_bytes: 100,
      free_disk_bytes: 1000, min_free_disk_bytes: 100, tenants: 2, max_tenants: 100 };
    if (label === "Free disk") admission.free_disk_bytes = reading;
    else if (label === "Available memory") admission.available_memory_bytes = reading;
    else admission.tenants = reading;
    const value = {
      ...health,
      tenants: { expected: 2, running: 2 },
      tenant_hubs: health.tenant_hubs.map((hub) => ({ ...hub, running: true })),
      admission,
    };
    await renderPlatform(fakeApi({ platformHealth: vi.fn(async () => value) }), "/platform/health");
    const panel = await screen.findByRole("region", { name: "Capacity" });
    const meter = within(panel).getByRole("meter", { name: label });
    expect(meter.getAttribute("aria-valuenow")).toBe(String(reading));
    const detail = document.getElementById(meter.getAttribute("aria-describedby")!);
    expect(detail?.textContent).toBe(expected);
    expect(detail?.className).toContain(expected === "Near limit" ? "text-warning-foreground" : expected === "At limit" ? "text-error-foreground" : "text-muted-foreground");
    expect(screen.getByRole("alert", { name: "Service health" }).getAttribute("data-variant")).toBe(expected === "At limit" ? "error" : expected === "Near limit" ? "warning" : "success");
  });

  it("refreshes health manually and every 30 seconds while visible, retaining the last reading on failure", async () => {
    const api = fakeApi();
    const read = vi.mocked(api.platformHealth);
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    const view = await renderPlatform(api, "/platform/health");
    await screen.findByRole("table", { name: "Tenant Hubs" });
    const before = read.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(read.mock.calls.length).toBe(before + 1));
    await waitFor(() => expect(screen.getByRole("button", { name: "Refresh" }).hasAttribute("disabled")).toBe(false));
    try {
      await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
      expect(read.mock.calls.length).toBe(before + 2);
      expect(screen.getByText("Updated 0s ago")).toBeTruthy();
      vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(read.mock.calls.length).toBe(before + 2);
      read.mockRejectedValueOnce(new AccountError({ status: 503, code: "unavailable", message: "Health read unavailable" }));
      vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
      await act(async () => { fireEvent(document, new Event("visibilitychange")); });
      expect(screen.getByText("Health read unavailable")).toBeTruthy();
      expect(screen.getByText("Updated 60s ago")).toBeTruthy();
      expect(screen.getByRole("table", { name: "Tenant Hubs" })).toBeTruthy();
      await act(async () => { await view.navigate({ to: "/platform/allowlist" }); });
      const stopped = read.mock.calls.length;
      await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
      expect(read.mock.calls.length).toBe(stopped);
    } finally {
      cleanup();
      vi.useRealTimers();
      vi.restoreAllMocks();
    }
  });

  it("refuses non-staff accounts", async () => {
    const refused = vi.fn(async () => {
      throw new AccountError({ status: 403, code: "forbidden", message: "limited" });
    });
    await renderPlatform(fakeApi({ platformOrganizations: refused }));
    expect((await screen.findByRole("alert")).textContent).toContain("limited to Detent staff");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it.each([
    ["session", "/platform/tenants"],
    ["organizations", "/platform/tenants"],
    ["platformOrganizations", "/platform/tenants"],
    ["platformHealth", "/platform/health"],
    ["platformAllowlist", "/platform/allowlist"],
    ["platformTenant", "/platform/tenants/org_alpha"],
  ] as const)("sends an expired %s session back to sign-in", async (method, path) => {
    const expired = vi.fn(async () => {
      throw new AccountError({ status: 401, code: "unauthenticated", message: "Sign in" });
    });
    await renderPlatform(fakeApi({ platformOrganizations: vi.fn(async () => administrator), [method]: expired }), path);
    await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM));
  });

  it.each(["alp", "org_alpha", "member@"])('finds a tenant by "%s" and opens its addressable detail', async (query) => {
    const user = userEvent.setup();
    const router = await renderPlatform(fakeApi());
    await screen.findByRole("table", { name: "Tenants" });
    fireEvent.keyDown(window, { key: "k", metaKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Find tenant or user" });
    const input = within(dialog).getByRole("combobox", { name: "Find tenant or user" });
    await user.type(input, query);
    await within(dialog).findByRole("option", { name: /Alpha/ });
    await user.keyboard("{Enter}");
    await waitFor(() => expect(router.state.location.pathname).toBe("/platform/tenants/org_alpha"));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Find tenant or user" })).toBeNull());
    await screen.findByRole("heading", { name: "Alpha" });
  });

  it("opens a selected user with results loaded without reloading the shell", async () => {
    const user = userEvent.setup();
    const api = fakeApi({ platformUsers: vi.fn(async () => ({ ...userResults, users: userResults.users.filter((user) => user.email.startsWith("alex")) })) });
    const router = await renderPlatform(api);
    await screen.findByRole("table", { name: "Tenants" });
    fireEvent.click(screen.getByRole("button", { name: /Find tenant or user/ }));
    const dialog = await screen.findByRole("dialog", { name: "Find tenant or user" });
    await user.type(within(dialog).getByRole("combobox", { name: "Find tenant or user" }), "alex@");
    await user.click(await within(dialog).findByRole("option", { name: /alex/ }));
    await screen.findByRole("region", { name: "Account" });
    expect(router.state.location.pathname).toBe("/platform/users/alex%2Btest%40example.test");
    expect(screen.getByRole("region", { name: "Organizations" }).textContent).toContain("No organization membership");
    expect(api.session).toHaveBeenCalledTimes(1);
  });

  it.each([
    [{ registry: { ok: true }, tenants: { expected: 2, running: 2 } }, "OK"],
    [{ registry: { ok: true }, tenants: { expected: 2, running: 1 } }, "Down"],
    [{ registry: { ok: false } }, "Down"],
  ])("renders the endpoint health status as %s", async (health, label) => {
    await renderPlatform(fakeApi({ platformHealth: vi.fn(async () => health) }));
    const nav = await screen.findByRole("navigation", { name: "Platform navigation" });
    expect(await within(nav).findByText(label)).toBeTruthy();
    expect(within(nav).getByText(label).getAttribute("data-slot")).toBe("badge");
    expect(await within(nav).findByText("2")).toBeTruthy();
    expect(within(nav).getByText("2").getAttribute("data-slot")).toBe("badge");
  });

  it("keeps the shell for a non-staff account without calling platform APIs", async () => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "customer@example.test", csrf: "c", platform_role: "" })) });
    await renderPlatform(api);
    await screen.findByText("You don't have access to this page");
    expect(screen.getByText("The platform console is limited to Detent staff.")).toBeTruthy();
    expect(api.platformOrganizations).not.toHaveBeenCalled();
    expect(api.platformHealth).not.toHaveBeenCalled();
  });

  it("formats byte readings", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(3 * 1024 ** 3)).toBe("3.0 GiB");
  });
});

describe("complimentary plans", () => {
  it.each(["create", "base", "subscription", "end_subscription"])(
    "submits an audited %s plan change from the tenant console",
    async (operation) => {
      const api = fakeApi({
        platformOrganizations: vi.fn(async () => administrator),
      });
      if (operation === "subscription" || operation === "end_subscription") {
        const detail = api.platformTenant;
        api.platformTenant = vi.fn(async (id) => {
          const value = await detail(id);
          return {
            ...value,
            organization: { ...value.organization, billing: { available: false } },
          };
        });
      }
      await renderPlatform(api);
      fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
      const plan = await screen.findByRole("region", {
        name: "Plan for Alpha",
      });
      fireEvent.click(
        within(plan).getByRole("button", {
          name:
            operation === "create"
              ? "Create plan version"
              : "Change base or subscription",
        }),
      );
      const dialog = await screen.findByRole("dialog", {
        name:
          operation === "create"
            ? "Create plan version for Alpha"
            : "Change base or subscription for Alpha",
      });
      fireEvent.click(
        within(dialog).getByRole("button", {
          name: operation === "create" ? "Create version" : "Apply plan",
        }),
      );
      expect((await within(dialog).findByRole("alert")).textContent).toContain(
        "Give a reason",
      );
      expect(api.changePlatformEntitlement).not.toHaveBeenCalled();
      if (operation === "create") {
        fireEvent.change(within(dialog).getByLabelText("Plan ID"), {
          target: { value: "extended_capacity" },
        });
        fireEvent.change(within(dialog).getByLabelText("projects"), {
          target: { value: "50" },
        });
      } else {
        fireEvent.change(within(dialog).getByLabelText("Plan"), {
          target: { value: "comp_team@1" },
        });
        if (operation === "base") {
          expect(
            within(dialog).queryByRole("option", { name: "Subscription" }),
          ).toBeNull();
          expect(
            within(dialog).queryByRole("option", { name: "End subscription" }),
          ).toBeNull();
        } else {
          fireEvent.change(within(dialog).getByLabelText("Apply as"), {
            target: { value: operation },
          });
          if (operation === "subscription") {
            fireEvent.change(within(dialog).getByLabelText("Expires"), {
              target: { value: "2027-01-31" },
            });
          }
        }
      }
      fireEvent.change(within(dialog).getByLabelText("Reason"), {
        target: { value: "Capacity adjustment" },
      });
      fireEvent.click(
        within(dialog).getByRole("button", {
          name: operation === "create" ? "Create version" : "Apply plan",
        }),
      );
      await waitFor(() =>
        expect(api.changePlatformEntitlement).toHaveBeenCalledTimes(1),
      );
      const call = vi.mocked(api.changePlatformEntitlement).mock.calls[0]![0];
      expect(call.organization).toBe("org_alpha");
      expect(call.change).toMatchObject({
        action: operation === "create" ? "create_plan" : operation,
        expected_revision: 3,
        reason: "Capacity adjustment",
        plan:
          operation === "create"
            ? { id: "extended_capacity", version: 2 }
            : { id: "comp_team", version: 1 },
      });
      if (call.change.action === "create_plan")
        expect(call.change.definition.allowances).toEqual({ projects: 50 });
      if (call.change.action === "subscription")
        expect(call.change.expires_at).toBe("2027-01-31T23:59:59Z");
      expect(call.change.idempotency_key).not.toBe("");
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    },
  );

  it("is hidden from staff who are not entitlement administrators", async () => {
    const api = fakeApi();
    await renderPlatform(api);
    await screen.findByRole("table", { name: "Tenants" });
    expect(screen.queryByRole("region", { name: "Complimentary plans" })).toBeNull();
    for (const name of ["Change plan", "Grant complimentary plan", "Grant model choice", "Create plan version", "Change base or subscription"]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
  });

  it.each(["admin", "billing"])("shows base, effective plan and active grants of ready organizations to %s", async (role) => {
    const api = fakeApi({
      session: vi.fn(async () => ({ email: "admin@example.com", csrf: "staff-csrf", platform_role: role })),
      platformOrganizations: vi.fn(async () => administrator),
    });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    await within(plan).findByText("Team");
    expect(within(plan).getByText("Complimentary")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Plan and billing" }));
    const billing = await screen.findByRole("region", { name: "Plan for Alpha" });
    const grants = await within(billing).findByRole("table", { name: "Grant history for Alpha" });
    expect(within(grants).getByText("design partner")).toBeTruthy();
    expect(within(grants).getByText("plans@example.com")).toBeTruthy();
    expect(within(grants).getByText(/31st December 2026/)).toBeTruthy();
    expect(within(grants).getByText("Hosted artifacts, Projects")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Plan for Beta" })).toBeNull();
    expect(api.platformEntitlements).toHaveBeenCalledWith("org_alpha");
  });

  it("requires a reason and posts the grant with the revision and an idempotency key", async () => {
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator) });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Change plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    expect(within(dialog).getByRole("combobox", { name: "Plan" }).textContent).toContain("Team");
    expect(dialog.textContent).not.toMatch(/pilot_free|comp_team|hosted_artifacts|\d{4}-\d{2}-\d{2}T/);
    fireEvent.change(within(dialog).getByLabelText("Expires (optional)"), { target: { value: "2026-12-31" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Give a reason");
    expect(api.changePlatformEntitlement).not.toHaveBeenCalled();
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "  design partner  " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    await waitFor(() => expect(api.changePlatformEntitlement).toHaveBeenCalledTimes(1));
    const call = vi.mocked(api.changePlatformEntitlement).mock.calls[0]![0];
    expect(call.organization).toBe("org_alpha");
    expect(call.csrf).toBe("staff-csrf");
    expect(call.change).toEqual({
      action: "grant",
      idempotency_key: expect.any(String),
      expected_revision: 3,
      plan: { id: "comp_team", version: 1 },
      expires_at: "2026-12-31T23:59:59Z",
      reason: "design partner",
    });
    expect(call.change.idempotency_key.length).toBeGreaterThan(0);
    await waitFor(() => expect(api.platformEntitlements).toHaveBeenCalledTimes(3));
  });

  it.each([
    {
      name: "a legacy grant stays offered as the current plan",
      grant: { id: "growth", version: 1 },
      options: ["Starter$19/mo", "Team$49/mo", "Growth$99/mo", "ScaleContact us", "Growth (legacy)Current$79/mo", "InternalContact us"],
      other: ["Growth (legacy)", "Internal"],
    },
    {
      name: "only the latest version of each plan is offered",
      grant: null,
      options: ["Starter$19/mo", "Team$49/mo", "Growth$99/mo", "ScaleContact us", "InternalContact us"],
      other: ["Internal"],
    },
    {
      name: "a current grant on the latest version is marked in place",
      grant: { id: "growth", version: 2 },
      options: ["Starter$19/mo", "Team$49/mo", "GrowthCurrent$99/mo", "ScaleContact us", "InternalContact us"],
      other: ["Internal"],
    },
  ])("orders grantable plans by tier: $name", async ({ grant, options, other }) => {
    const plan = (id: string, version: number, name: string, cents: number | undefined, slots: number, features = ["collaboration", "github_integration", "hosted_artifacts", "native_execution"]) =>
      ({ id, version, name, ...(cents === undefined ? {} : { monthly_usd_cents: cents }), features, allowances: { concurrent_work: slots } });
    const api = fakeApi({
      platformOrganizations: vi.fn(async () => administrator),
      platformEntitlements: vi.fn(async () => ({
        ...entitlements,
        base: { id: "free", version: 1 },
        effective_base: { id: "free", version: 1 },
        grants: grant === null ? [] : [{ ...entitlements.grants[0]!, plan: grant }],
        plans: [
          plan("internal", 1, "Internal", undefined, 50, ["collaboration", "model_choice"]),
          plan("scale", 2, "Scale", undefined, 25),
          plan("growth", 1, "Growth (legacy)", 7900, 8),
          plan("growth", 2, "Growth", 9900, 10),
          plan("team", 1, "Team (legacy)", 2900, 3),
          plan("team", 2, "Team", 4900, 5),
          plan("free", 1, "Free", 0, 1, ["collaboration"]),
          plan("starter", 1, "Starter", 1900, 2),
          ...entitlements.plans,
        ],
      })),
    });
    const user = userEvent.setup();
    await renderPlatform(api, "/platform/tenants/org_alpha");
    const region = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(region).findByRole("button", { name: "Change plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^Grant / });
    const facts = () => within(within(dialog).getByLabelText("Selected plan")).getAllByRole("definition").map((fact) => fact.textContent);
    const features = ["Collaboration", "GitHub", "Artifact storage", "Runner work"];
    expect(facts()).toEqual(["$19/mo", "2 runner slots", ...features]);
    await user.click(within(dialog).getByRole("combobox", { name: "Plan" }));
    expect((await screen.findAllByRole("option")).map((option) => option.textContent)).toEqual(options);
    expect(within(screen.getByRole("group", { name: "Other" })).getAllByRole("option").map((option) => option.textContent?.replace(/Current|Contact us|\$\d+\/mo/g, ""))).toEqual(other);
    await user.click(screen.getByRole("option", { name: /^Scale/ }));
    expect(facts()).toEqual(["Contact us", "25 runner slots", ...features]);
    if (grant?.version === 1) {
      await user.click(within(dialog).getByRole("combobox", { name: "Plan" }));
      await user.click(await screen.findByRole("option", { name: /^Growth \(legacy\)/ }));
      expect(facts()).toEqual(["$79/mo", "8 runner slots", "Current plan", ...features]);
      fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "extend pilot" } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
      await waitFor(() => expect(vi.mocked(api.changePlatformEntitlement).mock.calls[0]![0].change).toMatchObject({ plan: { id: "growth", version: 1 } }));
    }
  });

  it("grants model choice without selecting a plan and disables the control after reload", async () => {
    let enabled = false;
    const api = fakeApi({
      platformOrganizations: vi.fn(async () => administrator),
      platformEntitlements: vi.fn(async () => ({ ...entitlements, features: enabled ? ["model_choice"] : [] })),
      changePlatformEntitlement: vi.fn(async () => {
        enabled = true;
        return { action: "grant", grant_id: "model_grant" };
      }),
    });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Tenant actions" }));
    await user.click(await screen.findByRole("menuitem", { name: "Grant model choice" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    expect(within(dialog).queryByLabelText("Plan")).toBeNull();
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "Approved model choice" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    await waitFor(() => expect(api.changePlatformEntitlement).toHaveBeenCalledWith({
      organization: "org_alpha", csrf: "staff-csrf",
      change: {
        action: "grant", feature: "model_choice", idempotency_key: expect.any(String),
        expected_revision: 3, expires_at: null, reason: "Approved model choice",
      },
    }));
    await within(plan).findByText("On");
    expect((within(plan).getByRole("button", { name: "Grant model choice" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it.each([
    { name: "an unconfirmed failure retries the same command", failure: new AccountError({ status: 503, code: "audit_unavailable", message: "retry" }), same: true },
    { name: "a stale revision starts a new command", failure: new AccountError({ status: 409, code: "revision_conflict", message: "changed" }), same: false },
  ])("$name", async ({ failure, same }) => {
    let calls = 0;
    const change = vi.fn(async () => {
      calls += 1;
      if (calls === 1) throw failure;
      return { action: "grant", grant_id: "grant_retry" };
    });
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator), changePlatformEntitlement: change });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Change plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "design partner" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    await within(dialog).findByRole("alert");
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    await waitFor(() => expect(change).toHaveBeenCalledTimes(2));
    const [first, second] = vi.mocked(change).mock.calls.map((call) => (call as unknown as [{ change: { idempotency_key: string; expected_revision: number } }])[0].change);
    expect(second!.idempotency_key === first!.idempotency_key).toBe(same);
    if (same) expect(second!.expected_revision).toBe(first!.expected_revision);
  });

  it("warns that a plan it could not reload may be out of date", async () => {
    let reads = 0;
    const entitlements = fakeApi({}).platformEntitlements;
    const api = fakeApi({
      platformOrganizations: vi.fn(async () => administrator),
      platformEntitlements: vi.fn(async (organization: string) => {
        reads += 1;
        if (reads > 2) throw new AccountError({ status: 503, code: "tenant_unavailable", message: "The organization's Hub is unavailable" });
        return entitlements(organization);
      }),
    });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Change plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "design partner" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect((await screen.findByRole("alert")).textContent).toContain("may be out of date");
  });

  it("shows the stale revision message and reloads the plan", async () => {
    const stale = vi.fn(async () => {
      throw new AccountError({ status: 409, code: "revision_conflict", message: "Resource has changed" });
    });
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator), changePlatformEntitlement: stale });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Change plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "design partner" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(STALE_PLAN_MESSAGE);
    await waitFor(() => expect(api.platformEntitlements).toHaveBeenCalledTimes(3));
  });

  it("revokes a grant only with a reason", async () => {
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator) });
    await renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Revoke grant" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    expect(within(dialog).getByRole("heading", { name: "Revoke Team for Alpha?" })).toBeTruthy();
    expect(dialog.textContent).not.toMatch(/pilot_free|comp_team|hosted_artifacts|\d{4}-\d{2}-\d{2}T/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke grant" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Give a reason");
    expect(api.changePlatformEntitlement).not.toHaveBeenCalled();
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "pilot ended" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke grant" }));
    await waitFor(() => expect(api.changePlatformEntitlement).toHaveBeenCalledTimes(1));
    expect(vi.mocked(api.changePlatformEntitlement).mock.calls[0]![0].change).toEqual({
      action: "revoke",
      idempotency_key: expect.any(String),
      expected_revision: 3,
      grant_id: "comp_existing",
      reason: "pilot ended",
    });
  });
});

describe("tenant discovery and detail", () => {
  it("filters name, id and creator and combines state and plan filters with sorting", async () => {
    const user = userEvent.setup();
    const values = [
      { ...organizations.organizations[0]!, plan: "growth", created_at: "2026-09-22T10:00:00Z" },
      { ...organizations.organizations[1]!, plan: "free" },
    ];
    await renderPlatform(fakeApi({ platformOrganizations: vi.fn(async () => ({ ...organizations, organizations: values })) }));
    const table = await screen.findByRole("table", { name: "Tenants" });
    await within(table).findByRole("link", { name: "Alpha" });
    const names = () => within(table).getAllByRole("row").slice(1).map((row) => within(row).getByRole("link").textContent);
    expect(names()).toEqual(["Alpha", "Beta"]);
    for (const [header, ascending, descending] of [
      ["Tenant", ["Alpha", "Beta"], ["Beta", "Alpha"]],
      ["Status", ["Beta", "Alpha"], ["Alpha", "Beta"]],
      ["Created", ["Alpha", "Beta"], ["Beta", "Alpha"]],
    ] as const) {
      fireEvent.click(within(table).getByRole("button", { name: header }));
      await waitFor(() => expect(names()).toEqual(ascending));
      expect(within(table).getByRole("columnheader", { name: header }).getAttribute("aria-sort")).toBe(header === "Created" ? "descending" : "ascending");
      fireEvent.click(within(table).getByRole("button", { name: header }));
      await waitFor(() => expect(names()).toEqual(descending));
      expect(within(table).getByRole("columnheader", { name: header }).getAttribute("aria-sort")).toBe(header === "Created" ? "ascending" : "descending");
    }
    const search = screen.getByRole("textbox", { name: "Search tenants" });
    for (const query of ["MEMBER", "org_alpha", "alpha"]) {
      fireEvent.change(search, { target: { value: query } });
      await waitFor(() => expect(names()).toEqual(["Alpha"]));
    }
    fireEvent.change(search, { target: { value: "" } });
    await user.click(screen.getByRole("combobox", { name: "Status" }));
    await user.click(await screen.findByRole("option", { name: "Ready" }));
    await user.click(screen.getByRole("combobox", { name: "Plan" }));
    await user.click(await screen.findByRole("option", { name: "Growth" }));
    expect(names()).toEqual(["Alpha"]);
    expect(screen.getByText("1 tenant")).toBeTruthy();
    await user.click(screen.getByRole("combobox", { name: "Plan" }));
    await user.click(await screen.findByRole("option", { name: "Free" }));
    expect(await screen.findByText("No tenants match")).toBeTruthy();
  });

  it("updates counts, empty results and unavailable cells", async () => {
    await renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    await within(table).findByRole("link", { name: "Alpha" });
    expect(within(table).getAllByText("Unavailable")).toHaveLength(8);
    expect(screen.getByText("2 tenants")).toBeTruthy();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "eve@" } });
    expect(await screen.findByText("1 tenant")).toBeTruthy();
    expect(within(table).queryByRole("link", { name: "Alpha" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "missing" } });
    expect(await screen.findByText("No tenants match")).toBeTruthy();

  });

  it.each(["viewer", "support", "billing", "admin"])("opens a tenant page with role-gated actions for %s", async (role) => {
    const api = creditApi({ session: vi.fn(async () => ({ email: "staff@example.test", csrf: "staff-csrf", platform_role: role })) });
    await renderPlatform(api, "/platform/tenants/org_alpha");
    const organization = await screen.findByRole("region", { name: "Organization" });
    expect(within(organization).getByRole("link", { name: "member@example.test" }).getAttribute("href")).toBe("/platform/users/member%40example.test");
    expect(screen.queryByRole("dialog", { name: "Alpha" })).toBeNull();
    expect(within(screen.getByRole("region", { name: "AI credits" })).getByText("$100.00")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Support access" }) !== null).toBe(role === "support" || role === "admin");
    for (const name of ["Change plan", "Grant model choice", "Revoke grant"]) expect(screen.queryByRole("button", { name }) !== null).toBe(role === "billing" || role === "admin");
    expect(screen.queryByRole("button", { name: "Grant AI credits" }) !== null).toBe(role === "billing" || role === "admin");
    expect(screen.queryByRole("button", { name: "Grant credits" }) !== null).toBe(role === "billing" || role === "admin");
    expect(screen.queryByRole("button", { name: "Resume provisioning" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Members 1" }));
    const members = await screen.findByRole("region", { name: "Members" });
    expect(within(members).getByRole("link", { name: "member@example.test" }).getAttribute("href")).toBe("/platform/users/member%40example.test");
    expect(within(members).getByText("Owner")).toBeTruthy();
  });

  it.each(["", "0", "-5", "1.0000001", "1e3", "Infinity"])("rejects invalid credit amount %s", async (amount) => {
    const api = creditApi();
    await renderPlatform(api, "/platform/tenants/org_alpha");
    fireEvent.click(await screen.findByRole("button", { name: "Grant AI credits" }));
    const dialog = await screen.findByRole("dialog", { name: "Grant AI credits" });
    fireEvent.change(within(dialog).getByLabelText("Amount (USD)"), { target: { value: amount } });
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "Pilot credits" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant credits" }));
    expect(await within(dialog).findByText(/Enter a positive dollar amount/)).toBeTruthy();
    expect(api.adjustPlatformAICredits).not.toHaveBeenCalled();
  });

  it.each([
    { role: "billing", amount: "25.00", amountMicros: 25_000_000, balance: 100_000_000, balanceText: "$100.00", spent: 750_000, spentText: "$0.75", after: 125_000_000, afterText: "$125.00", submit: "Grant $25.00", adjustmentText: "+$25.00" },
    { role: "admin", amount: "25.00", amountMicros: 25_000_000, balance: 100_000_000, balanceText: "$100.00", spent: 750_000, spentText: "$0.75", after: 125_000_000, afterText: "$125.00", submit: "Grant $25.00", adjustmentText: "+$25.00" },
    { role: "billing", amount: "0.000001", amountMicros: 1, balance: 100_000_001, balanceText: "$100.000001", spent: 1, spentText: "$0.000001", after: 100_000_002, afterText: "$100.000002", submit: "Grant $0.000001", adjustmentText: "+$0.000001" },
    { role: "admin", amount: "0.001234", amountMicros: 1234, balance: 100_000_001, balanceText: "$100.000001", spent: 1, spentText: "$0.000001", after: 100_001_235, afterText: "$100.001235", submit: "Grant $0.001234", adjustmentText: "+$0.001234" },
  ])("grants and refreshes credit balance and history in place for $role ($amount)", async ({ role, amount, amountMicros, balance, balanceText, spent, spentText, after, afterText, submit, adjustmentText }) => {
    const api = creditApi({ session: vi.fn(async () => ({ email: "staff@example.test", csrf: "staff-csrf", platform_role: role })) });
    const initial = api.platformTenant;
    let applied = false;
    api.platformTenant = vi.fn(async (id) => ({ ...await initial(id), credit_balance_micros: applied ? after : balance,
      ai_credits: { ...credits, balance_micros: applied ? after : balance, spent_micros: spent, history: applied ? [{ ...credits.history[0]!, amount_micros: amountMicros, reason: "Extra pilot credits" }, ...credits.history] : credits.history } }));
    api.adjustPlatformAICredits = vi.fn(async () => { applied = true; return { organization_id: "org_alpha", balance_micros: after }; });
    const router = await renderPlatform(api, "/platform/tenants/org_alpha");
    const section = await screen.findByRole("region", { name: "AI credits" });
    expect(within(section).getByText("Spent in October")).toBeTruthy();
    expect(within(section).getByText(spentText)).toBeTruthy();
    expect(within(section).getByText("Off")).toBeTruthy();
    fireEvent.click(within(section).getByRole("button", { name: "Grant credits" }));
    const dialog = await screen.findByRole("dialog", { name: "Grant AI credits" });
    expect(within(dialog).getByText(balanceText)).toBeTruthy();
    if (amount === "25.00") fireEvent.click(within(dialog).getByRole("button", { name: "$25" }));
    else fireEvent.change(within(dialog).getByLabelText("Amount (USD)"), { target: { value: amount } });
    expect(within(dialog).getByText(afterText)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: submit }));
    expect(await within(dialog).findByText("Give a reason for this adjustment.")).toBeTruthy();
    expect(api.adjustPlatformAICredits).not.toHaveBeenCalled();
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: " Extra pilot credits " } });
    fireEvent.click(within(dialog).getByRole("button", { name: submit }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(api.adjustPlatformAICredits).toHaveBeenCalledWith({ organization: "org_alpha", csrf: "staff-csrf", change: { amount_usd: amount, reason: "Extra pilot credits", idempotency_key: expect.any(String) } });
    expect(within(screen.getByRole("region", { name: "AI credits" })).getByText(afterText)).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "AI credits" })).getByText(/Extra pilot credits/)).toBeTruthy();
    expect(api.platformAudit).toHaveBeenCalledTimes(2);
    expect(router.state.location.pathname).toBe("/platform/tenants/org_alpha");
    fireEvent.click(screen.getByRole("link", { name: "All adjustments" }));
    const history = await screen.findByRole("region", { name: "Adjustment history" });
    expect(within(history).getByText(adjustmentText, { exact: false })).toBeTruthy();
    expect(within(history).getByText("Purchased")).toBeTruthy();
    expect(within(history).getByText(/Pilot credits/)).toBeTruthy();
    expect(api.resumePlatformTenant).not.toHaveBeenCalled();
  });

  it.each([
    { amount: "10.00", preview: "$90.00", submit: "Remove $10.00", payload: "-10.00" },
    { amount: "0.000001", preview: "$99.999999", submit: "Remove $0.000001", payload: "-0.000001" },
  ])("replays an unknown removal of $amount with the same key after reopening and gives an edited command a fresh key", async ({ amount, preview, submit, payload }) => {
    const adjust = vi.fn().mockRejectedValue(new AccountError({ status: 502, code: "tenant_unavailable", message: "Hub unavailable" }));
    const api = creditApi({ adjustPlatformAICredits: adjust });
    await renderPlatform(api, "/platform/tenants/org_alpha?section=credits");
    fireEvent.click(await screen.findByRole("button", { name: "Grant AI credits" }));
    let dialog = await screen.findByRole("dialog", { name: "Grant AI credits" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove credits" }));
    fireEvent.change(within(dialog).getByLabelText("Amount (USD)"), { target: { value: amount } });
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "Correct duplicate grant" } });
    expect(within(dialog).getByText(preview)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: submit }));
    await within(dialog).findByText(/The result is unknown/);
    expect(adjust.mock.calls[0]![0].change.amount_usd).toBe(payload);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Grant AI credits" }));
    dialog = await screen.findByRole("dialog", { name: "Grant AI credits" });
    fireEvent.click(within(dialog).getByRole("button", { name: submit }));
    await waitFor(() => expect(adjust).toHaveBeenCalledTimes(2));
    await within(dialog).findByText(/The result is unknown/);
    expect(adjust.mock.calls[1]![0]).toEqual(adjust.mock.calls[0]![0]);
    fireEvent.click(within(dialog).getByRole("button", { name: "$25" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove $25.00" }));
    await waitFor(() => expect(adjust).toHaveBeenCalledTimes(3));
    expect(adjust.mock.calls[2]![0].change.idempotency_key).not.toBe(adjust.mock.calls[0]![0].change.idempotency_key);
  });

  it("renders unavailable credit history and disables grants without a balance", async () => {
    const api = fakeApi();
    await renderPlatform(api, "/platform/tenants/org_alpha?section=credits");
    expect(await screen.findByText("History unavailable")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Grant AI credits" }));
    const dialog = await screen.findByRole("dialog", { name: "Grant AI credits" });
    expect((within(dialog).getByRole("button", { name: "Grant credits" }) as HTMLButtonElement).disabled).toBe(true);
    expect(api.adjustPlatformAICredits).not.toHaveBeenCalled();
  });

  it("returns keyboard focus to the credit action after cancelling", async () => {
    const user = userEvent.setup();
    await renderPlatform(creditApi(), "/platform/tenants/org_alpha");
    const trigger = await screen.findByRole("button", { name: "Grant AI credits" });
    await user.click(trigger);
    const dialog = await screen.findByRole("dialog", { name: "Grant AI credits" });
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  it("restores URL sections, navigates sections and redirects old tenant links", async () => {
    const router = await renderPlatform(fakeApi(), "/platform/tenants/org_alpha?section=runners");
    const runners = await screen.findByRole("region", { name: "Runners" });
    expect(within(runners).getByText("Mac Studio")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Runners 1" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Projects 1" }));
    await screen.findByRole("region", { name: "Projects" });
    expect(router.state.location.search).toMatchObject({ section: "projects" });
    await act(() => router.navigate({ href: "/platform/tenants?tenant=org_alpha" } as never));
    await screen.findByRole("region", { name: "Organization" });
    expect(router.state.location.pathname).toBe("/platform/tenants/org_alpha");
  });

  it("disables plan grants when no other plan exists and explains why", async () => {
    const api = fakeApi({ platformEntitlements: vi.fn(async () => ({ ...entitlements, plans: [entitlements.plans[0]!] })) });
    await renderPlatform(api, "/platform/tenants/org_alpha");
    const grant = await screen.findByRole("button", { name: "Change plan" });
    expect((grant as HTMLButtonElement).disabled).toBe(true);
    fireEvent.keyDown(document, { key: "Tab" });
    act(() => grant.parentElement!.focus());
    expect(await screen.findByText("No other plan is configured to grant.")).toBeTruthy();
    expect(api.changePlatformEntitlement).not.toHaveBeenCalled();
  });

  it.each(["base", "subscription"] as const)("formats tenant labels, usage and grant return plans with a %s base", async (source) => {
    const api = fakeApi({
      platformEntitlements: vi.fn(async () => ({
        ...entitlements,
        source,
        effective_base: source === "subscription" ? { id: "paid_business", version: 1 } : entitlements.base,
        plans: [...entitlements.plans.map((plan) => ({ ...plan, name: plan.id === "pilot_free" ? "Starter" : "Team" })),
          { id: "paid_business", version: 1, name: "Business", features: ["collaboration"], allowances: { projects: 30, members: 20, concurrent_work: 10 } }],
        grant_history: [
          ...entitlements.grants,
          { ...entitlements.grants[0]!, id: "comp_models", scope: ["model_choice"], revoked_at: "2026-10-01T10:00:00Z" },
        ],
      })),
    });
    const original = api.platformTenant;
    api.platformTenant = vi.fn(async (id) => ({ ...await original(id), usage: { window_ends_at: "2026-10-10T15:00:00Z", allowances: { projects: { used: 2, limit: 20 }, members: { used: 3, limit: null }, concurrent_work: { used: 2, limit: null }, collaboration_bytes: { used: 390 * 1024 ** 2, limit: 1024 ** 3 } } } }));
    const auditRow = { at: "2026-10-09T10:00:00Z", actor: "staff@example.test", organization_id: "org_alpha", organization_name: "Alpha", organization_deleted: false, source: "entry" };
    api.platformAudit = vi.fn(async () => ({ rows: [
      { ...auditRow, event: "plan_granted", detail: "comp_team v1 until 2026-12-31T23:59:59Z", id: "one" },
      { ...auditRow, event: "support_requested", detail: "customer-request · Ticket 123", id: "two" },
      { ...auditRow, event: "organization_ready", detail: "generation 7", id: "three" },
    ], events: [], tenants: [], next_cursor: "" }));
    await renderPlatform(api, "/platform/tenants/org_alpha");
    const rawValues = /pilot_free|comp_team|paid_business|hosted_artifacts|model_choice|collaboration_bytes|artifact_retained_bytes|plan_granted|support_requested|organization_ready|customer-request|\d{4}-\d{2}-\d{2}T|generation/;
    expect((await screen.findByRole("meter", { name: "Projects" })).getAttribute("aria-valuetext")).toBe("2 of 20");
    expect(screen.getByRole("meter", { name: "Workspace data" }).getAttribute("aria-valuetext")).toBe("390 MB of 1 GB");
    expect(screen.getByText("Not included on Team")).toBeTruthy();
    expect(screen.getByRole("meter", { name: "Members" }).getAttribute("aria-valuetext")).toBe("3 used · no plan limit");
    expect(screen.getByRole("meter", { name: "Concurrent work" }).getAttribute("aria-valuetext")).toBe("2 used · no plan limit");
    await waitFor(() => expect(api.platformAudit).toHaveBeenCalledWith("tenant=org_alpha&limit=3"));
    const activity = await screen.findByRole("region", { name: "Recent activity" });
    expect(within(activity).getByText("Plan granted")).toBeTruthy();
    expect(within(activity).getByText(/Customer asked for help · Ticket 123/)).toBeTruthy();
    expect(within(activity).getByText("Organization ready")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Organization" }).textContent).toContain("September 2026");
    expect(screen.getByRole("region", { name: "Plan for Alpha" }).textContent).toContain(`Returns to ${source === "subscription" ? "Business" : "Starter"}`);
    expect(document.body.textContent).not.toMatch(rawValues);
    expect(within(activity).getByRole("link", { name: "Open audit log" }).getAttribute("href")).toBe("/platform/audit?tenant=org_alpha");
    fireEvent.click(screen.getByRole("button", { name: "Plan and billing" }));
    const history = await screen.findByRole("table", { name: "Grant history for Alpha" });
    expect(within(history).getByRole("cell", { name: "Team" })).toBeTruthy();
    expect(within(history).getByRole("cell", { name: "Hosted artifacts, Projects" })).toBeTruthy();
    expect(within(history).getAllByRole("cell", { name: "Model choice" })).toHaveLength(2);
    expect(within(history).getByRole("cell", { name: "Revoked" })).toBeTruthy();
    expect(history.textContent).toContain("September 2026");
    expect(document.body.textContent).not.toMatch(rawValues);
    fireEvent.click(screen.getByRole("button", { name: "Activity" }));
    const audit = await screen.findByRole("region", { name: "Audit activity" });
    await within(audit).findByText("Plan granted");
    expect(within(audit).getByText(/Customer asked for help · Ticket 123/)).toBeTruthy();
    const timeline = screen.getByRole("region", { name: "Provisioning timeline" });
    expect(within(timeline).getByText("Requested")).toBeTruthy();
    expect(timeline.textContent).toContain("September 2026");
    expect(document.body.textContent).not.toMatch(rawValues);
  });

  it("renders unavailable sections independently and confirms failed tenant resume", async () => {
    const user = userEvent.setup();
    const api = fakeApi();
    const original = api.platformTenant;
    api.platformTenant = vi.fn(async (id) => ({ ...await original(id), members: null, projects: null }));
    await renderPlatform(api, "/platform/tenants/org_beta?section=members");
    expect((await screen.findByRole("region", { name: "Members" })).textContent).toContain("Unavailable");
    fireEvent.click(screen.getByRole("button", { name: "Projects" }));
    expect((await screen.findByRole("region", { name: "Projects" })).textContent).toContain("Unavailable");
    await user.click(screen.getByRole("button", { name: "Tenant actions" }));
    await user.click(await screen.findByRole("menuitem", { name: "Resume provisioning" }));
    const confirmation = await screen.findByRole("alertdialog");
    expect(api.resumePlatformTenant).not.toHaveBeenCalled();
    fireEvent.click(within(confirmation).getByRole("button", { name: "Resume" }));
    await waitFor(() => expect(api.resumePlatformTenant).toHaveBeenCalledWith({ organization: "org_beta", csrf: "staff-csrf" }));
  });

});

if (typeof globalThis.PointerEvent === "undefined") globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;


const member = (email: string, role: string, bootstrap = false) => ({ email, role, bootstrap,
  added_by: bootstrap ? "bootstrap" : "admin@example.test", added_at: "2026-10-07T12:00:00Z", updated_at: "2026-10-09T15:00:00Z" });
const listing: PlatformMembers = { revision: 4, self: { email: "admin@example.test", role: "admin" }, members: [
  member("admin@example.test", "admin"), member("bootstrap@example.test", "admin", true), member("support@example.test", "support"),
] };

async function openStaff(overrides: Partial<EntryApi> = {}) {
  let current = listing;
  const api: EntryApi = { ...makeEntryApi(),
    session: vi.fn(async () => ({ email: "admin@example.test", csrf: "staff-csrf", platform_role: "admin" })),
    organizations: vi.fn(async () => ({ email: "admin@example.test", csrf: "staff-csrf", organizations: [] })),
    platformMembers: vi.fn(async () => current),
    changePlatformMember: vi.fn(async ({ change }) => {
      const members = change.action === "add" ? [...current.members, member(change.email, change.role!)] :
        change.action === "remove" ? current.members.filter((member) => member.email !== change.email) :
          current.members.map((member) => member.email === change.email ? { ...member, role: change.role! } : member);
      current = { ...current, members, revision: current.revision + 1 };
      return { email: change.email, role: change.role ?? "", revision: current.revision };
    }), ...overrides,
  };
  await renderPlatform(api, "/platform/staff");
  return api;
}

async function selectRole(label: string, role: string) {
  await userEvent.click(screen.getByRole("combobox", { name: label }));
  await userEvent.click(await screen.findByRole("option", { name: role }));
}

function reason(dialog: HTMLElement, value = "  staffing change  ") {
  fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value } });
}

describe("platform staff", () => {
  it("adds a normalized email with a reason and uses the refreshed revision for removal", async () => {
    const api = await openStaff();
    const staff = await screen.findByRole("region", { name: "Staff members" });
    expect(within(staff).getByText("3")).toBeTruthy();
    const roles = screen.getByRole("region", { name: "Roles" });
    for (const role of ["Admin", "Billing", "Support", "Viewer"]) {
      expect(within(roles).getByRole("heading", { name: role })).toBeTruthy();
    }
    for (const description of ["Everything, including staff, plans and AI credits.", "Plans, complimentary grants and AI credits.", "Support access into tenants.", "Read-only access to this console."]) {
      expect(within(roles).getByText(description)).toBeTruthy();
    }
    fireEvent.click(screen.getByRole("button", { name: "Add staff member" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("combobox", { name: "Role" }).textContent).toContain("Viewer");
    fireEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain("valid email");
    fireEvent.change(within(dialog).getByLabelText("Email"), { target: { value: " New@Example.test " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Give a reason");
    expect(api.changePlatformMember).not.toHaveBeenCalled();
    reason(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    await waitFor(() => expect(api.changePlatformMember).toHaveBeenCalledWith({ csrf: "staff-csrf", change: {
      action: "add", email: "new@example.test", role: "viewer", reason: "staffing change", expected_revision: 4, idempotency_key: expect.any(String),
    } }));
    const row = await screen.findByRole("group", { name: "new@example.test" });
    fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    const removal = await screen.findByRole("alertdialog");
    fireEvent.click(within(removal).getByRole("button", { name: "Remove member" }));
    expect((await within(removal).findByRole("alert")).textContent).toContain("Give a reason");
    reason(removal);
    fireEvent.click(within(removal).getByRole("button", { name: "Remove member" }));
    await waitFor(() => expect(api.changePlatformMember).toHaveBeenLastCalledWith({ csrf: "staff-csrf", change: {
      action: "remove", email: "new@example.test", reason: "staffing change", expected_revision: 5, idempotency_key: expect.any(String),
    } }));
    await waitFor(() => expect(screen.queryByText("new@example.test")).toBeNull());
  });

  it("keeps the old role until confirmation, supports cancellation and requires a reason", async () => {
    const api = await openStaff();
    await screen.findByRole("region", { name: "Staff members" });
    await selectRole("Role for support@example.test", "Billing");
    let dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading").textContent).toBe("Change role for support@example.test from Support to Billing");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("combobox", { name: "Role for support@example.test" }).textContent).toContain("Support");
    expect(api.changePlatformMember).not.toHaveBeenCalled();
    await selectRole("Role for support@example.test", "Billing");
    dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Change role" }));
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Give a reason");
    reason(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Change role" }));
    await waitFor(() => expect(api.changePlatformMember).toHaveBeenCalledWith({ csrf: "staff-csrf", change: {
      action: "change", email: "support@example.test", role: "billing", reason: "staffing change", expected_revision: 4, idempotency_key: expect.any(String),
    } }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Role for support@example.test" }).textContent).toContain("Billing"));
  });

  it.each([
    { name: "own", value: listing, email: "admin@example.test", tooltip: "You can't change your own role" },
    { name: "bootstrap", value: listing, email: "bootstrap@example.test", tooltip: "The bootstrap admin cannot be demoted" },
    { name: "last admin", value: { ...listing, self: { email: "reader@example.test", role: "admin" }, members: [member("last@example.test", "admin")] }, email: "last@example.test", tooltip: "At least one admin is required" },
  ])("protects the $name row", async ({ value, email, tooltip }) => {
    await openStaff({ platformMembers: vi.fn(async () => value) });
    const row = await screen.findByRole("group", { name: email });
    expect(row.textContent).toContain(`Added 2026-10-07 by ${value.members.find((member) => member.email === email)!.added_by} · changed 2026-10-09`);
    if (value.self.email === email) {
      expect(within(row).queryByRole("combobox")).toBeNull();
      expect(within(row).getByText("Admin")).toBeTruthy();
      expect(within(row).getByText("You")).toBeTruthy();
      expect(within(row).getByText(tooltip)).toBeTruthy();
    } else {
      expect((within(row).getByRole("combobox") as HTMLButtonElement).disabled).toBe(true);
      expect(within(row).getByLabelText(tooltip)).toBeTruthy();
    }
    expect(within(row).queryByRole("button", { name: "Remove" })).toBeNull();
  });

  it("shows a duplicate error inline and allows correction", async () => {
    const api = await openStaff({ changePlatformMember: vi.fn(async () => { throw new AccountError({ status: 409, code: "already_member", message: "Already a member" }); }) });
    await screen.findByRole("region", { name: "Staff members" });
    fireEvent.click(screen.getByRole("button", { name: "Add staff member" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Email"), { target: { value: "support@example.test" } });
    reason(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe("Already a member");
    expect((within(dialog).getByLabelText("Email") as HTMLInputElement).disabled).toBe(false);
    expect(api.platformMembers).toHaveBeenCalledTimes(1);
  });

  it("shows the stale toast, blocks edits and reloads the staff list", async () => {
    let current = listing;
    const api = await openStaff({ platformMembers: vi.fn(async () => current), changePlatformMember: vi.fn(async () => {
      current = { ...listing, revision: 7, members: listing.members.map((member) => member.email === "support@example.test" ? { ...member, role: "viewer" } : member) };
      throw new AccountError({ status: 409, code: "revision_conflict", message: "changed" });
    }) });
    await screen.findByRole("region", { name: "Staff members" });
    await selectRole("Role for support@example.test", "Billing");
    const dialog = await screen.findByRole("dialog");
    reason(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Change role" }));
    await screen.findByText(STALE_STAFF_MESSAGE);
    expect((screen.getByRole("button", { name: "Add staff member" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(api.platformMembers).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Role for support@example.test" }).textContent).toContain("Viewer"));
    expect((screen.getByRole("button", { name: "Add staff member" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("retries an unconfirmed write with the same key and input", async () => {
    const write = vi.fn(async () => { throw new AccountError({ status: 503, code: "unavailable", message: "Retry to verify" }); });
    await openStaff({ changePlatformMember: write });
    await screen.findByRole("region", { name: "Staff members" });
    await selectRole("Role for support@example.test", "Billing");
    const dialog = await screen.findByRole("dialog");
    reason(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Change role" }));
    await within(dialog).findByRole("alert");
    expect((within(dialog).getByLabelText("Reason") as HTMLTextAreaElement).disabled).toBe(true);
    fireEvent.click(within(dialog).getByRole("button", { name: "Change role" }));
    await waitFor(() => expect(write).toHaveBeenCalledTimes(2));
    expect(write.mock.calls[1]).toEqual(write.mock.calls[0]);

  });
});

const audit: PlatformAudit = {
  rows: [
    {
      id: "1",
      source: "audit",
      at: "2026-10-01T12:00:00Z",
      actor: "admin@example.com",
      event: "support_started",
      organization_id: "org_alpha",
      organization_name: "Alpha",
      organization_deleted: false,
      detail: "troubleshooting",
    },
    {
      id: "1",
      source: "organization_events",
      at: "2026-09-30T12:00:00Z",
      actor: "system",
      event: "organization_ready",
      organization_id: "org_beta",
      organization_name: "",
      organization_deleted: true,
      detail: "generation 1",
    },
  ],
  events: ["organization_ready", "support_started"],
  tenants: [
    { id: "org_alpha", name: "Alpha", deleted: false },
    { id: "org_beta", name: "Beta", deleted: true },
  ],
  next_cursor: "older-cursor",
};

describe("platform audit", () => {
  it("restores shared filters, updates the URL, clears and handles history", async () => {
    const api = fakeApi({ platformAudit: vi.fn(async () => audit) });
    const router = await renderPlatform(
      api,
      "/platform/audit?tenant=org_alpha&actor=admin&event=support_started&from=2026-09-01&to=2026-10-01",
    );
    await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    expect(document.title).toBe("Audit log · Platform · Detent");
    fireEvent.click(screen.getByRole("button", { name: /^Actor:/ }));
    expect((screen.getByLabelText("Actor") as HTMLInputElement).value).toBe(
      "admin",
    );
    fireEvent.click(screen.getByRole("button", { name: /^Tenant:/ }));
    await waitFor(() =>
      expect((screen.getByLabelText("Tenant") as HTMLInputElement).value).toBe(
        "Alpha",
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: /^Action:/ }));
    expect(screen.getByLabelText("Action").textContent).toBe("Support access started");
    fireEvent.click(screen.getByRole("button", { name: /2026-09-01 – 2026-10-01/ }));
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe(
      "2026-09-01",
    );
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe(
      "2026-10-01",
    );
    expect(api.platformAudit).toHaveBeenCalledWith(
      "tenant=org_alpha&actor=admin&event=support_started&from=2026-09-01&to=2026-10-01",
    );
    fireEvent.click(screen.getByRole("button", { name: /^Actor:/ }));
    fireEvent.change(screen.getByLabelText("Actor"), {
      target: { value: "system" },
    });
    await waitFor(() =>
      expect(router.state.location.searchStr).toContain("actor=system"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    await waitFor(() => expect(router.state.location.searchStr).toBe(""));
    expect(screen.getByRole("button", { name: "Actor: Anyone" })).toBeTruthy();
    await act(async () => {
      router.history.back();
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Actor: system" })).toBeTruthy(),
    );
    await act(async () => {
      router.history.forward();
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Actor: Anyone" })).toBeTruthy(),
    );
  });

  it("uses searchable tenant and stored event choices in the URL", async () => {
    const router = await renderPlatform(
      fakeApi({ platformAudit: vi.fn(async () => audit) }),
      "/platform/audit",
    );
    await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    fireEvent.click(screen.getByRole("button", { name: /^Action:/ }));
    fireEvent.click(screen.getByLabelText("Action"));
    await userEvent.click(
      await screen.findByRole("option", { name: "Tenant ready" }),
    );
    await waitFor(() =>
      expect(router.state.location.searchStr).toBe("?event=organization_ready"),
    );
    fireEvent.click(screen.getByRole("button", { name: /^Tenant:/ }));
    const tenant = screen.getByLabelText("Tenant");
    await userEvent.click(tenant);
    await userEvent.clear(tenant);
    await userEvent.type(tenant, "Beta");
    await userEvent.click(
      await screen.findByRole("option", { name: "Beta (deleted)" }),
    );
    await waitFor(() =>
      expect(router.state.location.searchStr).toBe(
        "?tenant=org_beta&event=organization_ready",
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove tenant filter" }));
    await waitFor(() => expect(router.state.location.searchStr).toBe("?event=organization_ready"));
    fireEvent.click(screen.getByRole("button", { name: "All dates" }));
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-10T23:00:00Z"));
    fireEvent.click(screen.getByRole("button", { name: "Last 7 days" }));
    vi.useRealTimers();
    await waitFor(() => expect(router.state.location.searchStr).toBe("?event=organization_ready&from=2026-10-04&to=2026-10-10"));

  });

  it("appends older rows, preserves tenant links and does not poll", async () => {
    const old = {
      ...audit.rows[0]!,
      id: "2",
      actor: "unresolved_subject",
      detail: "older reason",
    };
    const api = fakeApi({
      platformAudit: vi
        .fn()
        .mockResolvedValueOnce(audit)
        .mockResolvedValueOnce({ ...audit, rows: [old], next_cursor: "" }),
    });
    await renderPlatform(api, "/platform/audit?actor=admin");
    const table = await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    expect(
      within(table).getByRole("link", { name: "Alpha" }).getAttribute("href"),
    ).toBe("/platform/tenants/org_alpha");
    expect(
      within(table)
        .getByRole("link", { name: "Beta" })
        .getAttribute("href"),
    ).toBe("/platform/tenants/org_beta");
    expect(within(table).getByText("Deleted")).toBeTruthy();
    expect(api.platformAudit).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Load older events" }));
    await screen.findByText("older reason");
    expect(within(table).getAllByRole("row")).toHaveLength(4);
    expect(within(table).getByText("Unknown account")).toBeTruthy();
    expect(table.textContent).not.toContain("unresolved_subject");
    expect(screen.getByLabelText("3 events loaded").textContent).toBe("3");
    expect(api.platformAudit).toHaveBeenLastCalledWith(
      "actor=admin&cursor=older-cursor",
    );
    expect(api.platformAudit).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: "Load older events" })).toBeNull();
  });

  it("drops an older page after changing filters and preserves rows on a failed retry", async () => {
    let resolveOlder!: (page: PlatformAudit) => void;
    const api = fakeApi({
      platformAudit: vi
        .fn()
        .mockResolvedValueOnce(audit)
        .mockImplementationOnce(
          () =>
            new Promise<PlatformAudit>((resolve) => {
              resolveOlder = resolve;
            }),
        )
        .mockResolvedValueOnce({ ...audit, rows: [audit.rows[1]!] })
        .mockRejectedValueOnce(
          new AccountError({
            status: 503,
            code: "unavailable",
            message: "Audit unavailable",
          }),
        )
        .mockResolvedValueOnce({
          ...audit,
          rows: [audit.rows[0]!],
          next_cursor: "",
        }),
    });
    await renderPlatform(api, "/platform/audit");
    await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    fireEvent.click(screen.getByRole("button", { name: "Load older events" }));
    expect(
      screen
        .getByRole("button", { name: "Load older events" })
        .hasAttribute("disabled"),
    ).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: /^Actor:/ }));
    fireEvent.change(screen.getByLabelText("Actor"), {
      target: { value: "system" },
    });
    await waitFor(() =>
      expect(api.platformAudit).toHaveBeenCalledWith("actor=system"),
    );
    await screen.findByText("Provisioning attempt 1");
    await act(async () => {
      resolveOlder({
        ...audit,
        rows: [{ ...audit.rows[0]!, detail: "stale older row" }],
      });
    });
    expect(screen.queryByText("stale older row")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Load older events" }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Audit unavailable",
    );
    expect(screen.getByText("Provisioning attempt 1")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Load older events" }));
    await screen.findByText("troubleshooting");
    expect(screen.getByText("Provisioning attempt 1")).toBeTruthy();
  });

  it.each([401, 403])(
    "handles an audit %s inside the platform shell",
    async (status) => {
      const error = new AccountError({
        status,
        code: "denied",
        message: "Audit denied",
      });
      await renderPlatform(
        fakeApi({
          platformAudit: vi.fn(async () => {
            throw error;
          }),
        }),
        "/platform/audit",
      );
      expect((await screen.findByRole("alert")).textContent).toBe(
        "Audit denied",
      );
      expect(screen.getByText("Platform")).toBeTruthy();
      if (status === 401)
        await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM));
    },
  );

  it("debounces actor typing and cancels a pending draft on clear", async () => {
    const api = fakeApi({ platformAudit: vi.fn(async () => ({ ...audit, next_cursor: "" })) });
    const router = await renderPlatform(api, "/platform/audit?tenant=org_alpha&event=support_started");
    await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    fireEvent.click(screen.getByRole("button", { name: "Actor: Anyone" }));
    const input = screen.getByLabelText("Actor");
    vi.useFakeTimers();
    fireEvent.change(input, { target: { value: "a" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    fireEvent.change(input, { target: { value: "admin" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(349); });
    expect(api.platformAudit).toHaveBeenCalledTimes(1);
    expect((screen.getByRole("button", { name: "Export CSV" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(api.platformAudit).toHaveBeenLastCalledWith("tenant=org_alpha&actor=admin&event=support_started");
    expect(api.platformAudit).toHaveBeenCalledTimes(2);
    expect(router.state.location.searchStr).toContain("actor=admin");
    fireEvent.change(input, { target: { value: "stale" } });
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(350); });
    expect(router.state.location.searchStr).toBe("");
    expect(api.platformAudit).not.toHaveBeenCalledWith(expect.stringContaining("stale"));
  });

  it.each([
    ["plan_granted", "Plan granted", "info"],
    ["support_started", "Support access started", "warning"],
    ["organization_failed", "Tenant failed", "error"],
    ["member_role_changed", "Staff role changed", "info"],
    ["platform_audit_viewed", "Platform audit viewed", "secondary"],
  ])("labels %s without showing the stored key", (event, label, variant) => {
    expect(auditAction(event)).toEqual({ label, variant });
  });

  it.each([
    ["person@example.test", "person@example.test"],
    ["system", "System"],
    ["platform_service_token_123", "Platform service token"],
    ["hosted_subject_123", "Unknown account"],
  ])("shows a readable actor for %s", (actor, label) => {
    expect(auditActor(actor)).toBe(label);
  });

  it("exports every cursor page with the active filters and readable CSV cells", async () => {
    const selected = { ...audit.rows[0]!, event: "plan_granted", detail: 'growth v1 until 2026-12-31T23:59:59Z · customer, "pilot"' };
    const old = { ...selected, id: "older", at: "2026-09-30T10:00:00Z", detail: "growth v1 · =customer reason" };
    const api = fakeApi({ platformAudit: vi.fn(async (query: string) => {
      const params = new URLSearchParams(query);
      if (params.get("tenant") !== "org_alpha" || params.get("actor") !== "admin" || params.get("event") !== "plan_granted" || params.get("from") !== "2026-09-01" || params.get("to") !== "2026-10-01") return { ...audit, rows: [audit.rows[1]!], next_cursor: "" };
      return params.has("cursor") ? { ...audit, rows: [old], next_cursor: "" } : { ...audit, rows: [selected] };
    }) });
    let exported!: Blob;
    const createObjectURL = vi.fn((blob: Blob) => { exported = blob; return "blob:audit"; });
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", class extends URL { static createObjectURL = createObjectURL; static revokeObjectURL = revokeObjectURL; });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    await renderPlatform(api, "/platform/audit?tenant=org_alpha&actor=admin&event=plan_granted&from=2026-09-01&to=2026-10-01");
    const table = await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    expect(table.textContent).toContain("Plan granted");
    expect(table.textContent).toContain("Growth (version 1) until Dec 31, 2026");
    expect(table.textContent).toContain('customer, "pilot"');
    expect(table.textContent).not.toContain("plan_granted");
    fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
    await waitFor(() => expect(click).toHaveBeenCalledTimes(1));
    const csv = await exported.text();
    expect(csv).toBe('"When (UTC)","Action","Tenant","By","Details","Reason"\r\n' +
      '"2026-10-01T12:00:00Z","Plan granted","Alpha","admin@example.com","Growth (version 1) until Dec 31, 2026","customer, ""pilot"""\r\n' +
      '"2026-09-30T10:00:00Z","Plan granted","Alpha","admin@example.com","Growth (version 1)","\'=customer reason"\r\n');
    expect(api.platformAudit).toHaveBeenNthCalledWith(2, "tenant=org_alpha&actor=admin&event=plan_granted&from=2026-09-01&to=2026-10-01&limit=200", expect.any(AbortSignal));
    expect(api.platformAudit).toHaveBeenLastCalledWith("tenant=org_alpha&actor=admin&event=plan_granted&from=2026-09-01&to=2026-10-01&limit=200&cursor=older-cursor", expect.any(AbortSignal));
    expect((click.mock.instances[0] as unknown as HTMLAnchorElement).download).toBe("audit-log.csv");
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:audit");
  });

  it.each([10_000, 10_001])("bounds an export of %s events without downloading a partial CSV", async (total) => {
    const api = fakeApi({ platformAudit: vi.fn(async (query: string) => {
      const params = new URLSearchParams(query);
      if (!params.has("limit")) return audit;
      expect(params.get("actor")).toBe("admin");
      expect(params.get("limit")).toBe("200");
      const offset = Number(params.get("cursor") ?? "0");
      const rows = Array.from({ length: Math.min(200, total - offset) }, (_, index) => ({ ...audit.rows[0]!, id: String(offset + index) }));
      return { ...audit, rows, next_cursor: offset + rows.length < total ? String(offset + rows.length) : "" };
    }) });
    let exported: Blob | undefined;
    const createObjectURL = vi.fn((blob: Blob) => { exported = blob; return "blob:audit"; });
    vi.stubGlobal("URL", class extends URL { static createObjectURL = createObjectURL; static revokeObjectURL = vi.fn(); });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    await renderPlatform(api, "/platform/audit?actor=admin");
    await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
    if (total === 10_000) {
      await waitFor(() => expect(click).toHaveBeenCalledTimes(1));
      const csv = await exported!.text();
      expect(csv.split("\r\n")).toHaveLength(10_002);
      expect(csv.match(/"When \(UTC\)"/g)).toHaveLength(1);
    } else {
      await screen.findByText("Export exceeds 10,000 events. Narrow the date range or other filters and try again.");
      expect(createObjectURL).not.toHaveBeenCalled();
      expect(click).not.toHaveBeenCalled();
    }
    expect(api.platformAudit).toHaveBeenCalledTimes(51);
    expect((screen.getByRole("button", { name: "Export CSV" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it.each(["cancel", "filters", "unmount"])("aborts an in-flight export on %s without continuing the crawl", async (stop) => {
    let signal: AbortSignal | undefined;
    let holdExport = true;
    const fetch = vi.fn(async (input: string, init?: RequestInit) => {
      const params = new URL(input, "https://platform.detent.test").searchParams;
      if (params.has("limit") && holdExport) {
        signal = init?.signal ?? undefined;
        return new Promise<Response>((_, reject) => {
          signal!.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true });
        });
      }
      return new Response(JSON.stringify({ ...audit, next_cursor: "" }), { headers: { "Content-Type": "application/json" } });
    });
    const api = fakeApi({ platformAudit: makeEntryApi({ fetch }).platformAudit });
    const createObjectURL = vi.fn(() => "blob:audit");
    vi.stubGlobal("URL", class extends URL { static createObjectURL = createObjectURL; static revokeObjectURL = vi.fn(); });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    await renderPlatform(api, "/platform/audit?actor=admin");
    await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
    await screen.findByRole("button", { name: "Cancel export" });
    expect(signal?.aborted).toBe(false);
    if (stop === "cancel") fireEvent.click(screen.getByRole("button", { name: "Cancel export" }));
    else if (stop === "filters") fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    else cleanup();
    await waitFor(() => expect(signal?.aborted).toBe(true));
    expect(createObjectURL).not.toHaveBeenCalled();
    expect(fetch.mock.calls.filter(([url]) => new URL(url, "https://platform.detent.test").searchParams.has("limit"))).toHaveLength(1);
    if (stop !== "unmount") {
      const button = await screen.findByRole("button", { name: "Export CSV" });
      expect(screen.queryByText("Aborted")).toBeNull();
      holdExport = false;
      fireEvent.click(button);
      await waitFor(() => expect(click).toHaveBeenCalledTimes(1));
    }
  });

  it("shows an empty state for filters with no matches", async () => {
    await renderPlatform(fakeApi(), "/platform/audit?actor=nobody");
    await screen.findByText("No events match these filters.");
    expect(screen.queryByRole("table")).toBeNull();
  });
});
