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
  email: "admin@detent.build",
  csrf: "staff-csrf",
  can_support: true,
  unavailable: ["plan", "grants", "member_count", "runner_count"],
  organizations: [
    {
      id: "org_alpha", name: "Alpha", state: "ready", step: "publish", attempts: 0, error_code: "", managed: true,
      creator_email: "dana@example.test", created_at: "2026-09-20T10:00:00Z", updated_at: "2026-09-20T10:05:00Z",
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
  allowed_emails: ["dana@example.test"],
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
      granted_by: "plans@detent.build", granted_at: "2026-09-20T10:00:00Z",
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
    organizations: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", organizations: [], pending: [], can_create: false, platform_role: "admin" })),
    session: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", can_create: false, platform_role: "admin" })),
    provisioning: vi.fn(async () => ({ id: "", name: "", state: "", step: "", error: "", can_resume: false })),
    createOrganization: vi.fn(async () => ({ next: "/" })),
    resume: vi.fn(async () => ({ next: "/" })),
    platformTenant: vi.fn(async (id: string) => ({
      organization: organizations.organizations.find((item) => item.id === id)!,
      csrf: "staff-csrf", can_grant: true, can_resume: id === "org_beta", unavailable: [],
      members: [{ id: "owner", email: "dana@example.test", role: "owner" }],
      runners: [{ id: "runner_one", name: "Mac Studio", health: "online" }],
      projects: [{ id: "prj_one", name: "Detent" }],
      events: [{ event: "requested", generation: 1, recorded_at: "2026-09-20T10:00:00Z" }],
      entitlements: await api.platformEntitlements(id),
    })),
    resumePlatformTenant: vi.fn(async () => ({ next: "/platform/tenants" })),
    platformUsers: vi.fn(async () => ({ users: [], unsearched: [] })),
    platformOrganizations: vi.fn(async () => organizations),
    platformAllowlist: vi.fn(async () => allowlist),
    platformMembers: vi.fn(async () => ({ members: [], revision: 1, self: { email: "admin@detent.build", role: "admin" } })),
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

function renderPlatform(api: EntryApi, path = "/platform/tenants") {
  const router = makeEntryRouter(createMemoryHistory({ initialEntries: [path] }));
  renderWith(api, <RouterProvider router={router} />);
  return router;
}

describe("platform console", () => {
  it("debounces URL-backed user search and rejects short queries", async () => {
    const search = vi.fn(async () => userResults);
    const router = renderPlatform(fakeApi({ platformUsers: search }), "/platform/users");
    const input = await screen.findByRole("searchbox", { name: "Search by email" });
    vi.useFakeTimers();
    fireEvent.change(input, { target: { value: "ab" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(300); });
    expect(search).not.toHaveBeenCalled();
    expect(router.state.location.search).toEqual({ q: "ab" });
    fireEvent.change(input, { target: { value: "example" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    fireEvent.change(input, { target: { value: " example.test " } });
    await act(async () => { await vi.advanceTimersByTimeAsync(299); });
    expect(search).not.toHaveBeenCalled();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
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
    const router = renderPlatform(api, "/platform/users?q=example.test");
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
    renderPlatform(api, selected);
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
    const router = renderPlatform(api, "/platform/users/sam%40example.test");
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
    renderPlatform(api, "/platform/users?q=example.test");
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
    renderPlatform(fakeApi({ platformUsers: search, session: vi.fn(async () => ({ email: "person@example.test", csrf: "c", platform_role: "" })) }), "/platform/users/sam%40example.test");
    expect(await screen.findByText("You don't have access to this page")).toBeTruthy();
    expect(search).not.toHaveBeenCalled();
    cleanup();
    renderPlatform(fakeApi({ platformUsers: search, session: vi.fn(async () => ({ email: "support@example.test", csrf: "c", platform_role: "support" })) }), "/platform/users/sam%40example.test");
    await screen.findByRole("region", { name: "Account" });
    expect(screen.queryByRole("link", { name: "Manage in Staff" })).toBeNull();
  });

  it.each([401, 403, 503])("handles user search failure %s", async (status) => {
    let refuse!: (error: AccountError) => void;
    const search = vi.fn().mockImplementationOnce(() => new Promise<typeof userResults>((_, reject) => { refuse = reject; })).mockResolvedValue(userResults);
    const api = fakeApi({ platformUsers: search });
    const user = userEvent.setup();
    const router = renderPlatform(api, "/platform/users");
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
    renderPlatform(api);
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
      email: "admin@detent.build", csrf: "c", platform_role: "admin",
      organizations: [
        { id: "org_alpha", name: "My Alpha", url: "/organizations/org_alpha/organization" },
        { id: "org_beta", name: "My Beta", url: "/organizations/org_beta/work" },
      ],
    })) });
    const router = renderPlatform(api);
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
    renderPlatform(api, "/platform/staff");
    expect(await screen.findByText("You don't have access to this page")).toBeTruthy();
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Staff" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add staff member" })).toBeNull();
    expect(api.platformMembers).not.toHaveBeenCalled();
  });

  it("lists newest tenants first with human status, owner and explicit unavailable values", async () => {
    renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    await within(table).findByRole("link", { name: "Alpha" });
    const rows = within(table).getAllByRole("row");
    expect(within(rows[1]!).getByText("Beta")).toBeTruthy();
    expect(within(rows[1]!).getByText("Failed")).toBeTruthy();
    expect(within(rows[1]!).getByText("Tenant files")).toBeTruthy();
    expect(within(rows[2]!).getByText("dana@example.test")).toBeTruthy();
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
    const router = renderPlatform(api, "/platform/tenants?q=dana&segment=complimentary&plan=starter&status=Ready&sort=name&direction=desc");
    const table = await screen.findByRole("table", { name: "Tenants" });
    const link = await within(table).findByRole("link", { name: "Alpha" });
    expect((screen.getByRole("textbox", { name: "Search tenants" }) as HTMLInputElement).value).toBe("dana");
    expect(screen.getByRole("combobox", { name: "Plan" }).textContent).toContain("Starter");
    expect(screen.getByRole("combobox", { name: "Status" }).textContent).toContain("Ready");
    expect(within(table).getByText("Until 2026-12-31")).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Complimentary/ }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("columnheader", { name: "Tenant" }).getAttribute("aria-sort")).toBe("descending");
    fireEvent.click(link);
    await screen.findByRole("heading", { name: "Alpha" });
    expect(router.state.location.pathname).toBe("/platform/tenants/org_alpha");
    expect(router.state.location.search).toMatchObject({ q: "dana", segment: "complimentary", plan: "starter", status: "Ready", sort: "name", direction: "desc" });
    fireEvent.click(screen.getByRole("link", { name: "← Back to tenants" }));
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
    renderPlatform(api, url);
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
    renderPlatform(api, "/platform/tenants?segment=attention");
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(await within(table).findByRole("link", { name: "Alpha" })).toBeTruthy();
    expect(within(table).getByRole("link", { name: "Beta" })).toBeTruthy();
    expect(within(table).getByText("0 online")).toBeTruthy();
    cleanup();
    api.platformTenant = vi.fn(async () => { throw new AccountError({ status: 503, code: "unavailable", message: "Unavailable" }); });
    renderPlatform(api);
    const incomplete = await screen.findByRole("table", { name: "Tenants" });
    expect(await within(incomplete).findByRole("link", { name: "Alpha" })).toBeTruthy();
    expect(screen.getByText(/Not shown: runner health/)).toBeTruthy();
  });

  it("keeps loading, failure, retry and empty states in the tenant list template", async () => {
    let reject: ((error: AccountError) => void) | undefined;
    const pending = new Promise<typeof organizations>((_resolve, fail) => { reject = fail; });
    const read = vi.fn(() => pending);
    renderPlatform(fakeApi({ platformOrganizations: read }));
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
    const detail = { organization, entitlements, runners: [], members: [], projects: [], events: [], unavailable: [], csrf: "c", can_grant: false, can_resume: false };
    expect(tenantRow(organization, detail).grantExpiry).toBe("2026-12-31T23:59:59Z");
    expect(tenantRow(organization, { ...detail, entitlements: { ...entitlements, grants: [{ ...entitlements.grants[0]!, expires_at: null }] } }).grantExpiry).toBeNull();
    const scheduled = tenantRow({ ...organization, runner_count: 2 }, {
      ...detail,
      runners: [{ id: "scheduled", name: "Scheduled", health: "outside_hours" }, { id: "offline", name: "Offline", health: "offline" }],
    });
    expect(scheduled.onlineRunners).toBe(1);
    expect(tenantNeedsAttention(scheduled)).toBe(false);
  });

  it("starts support access through the entry form with a required reason and the CSRF token", async () => {
    let resolveOrganizations!: (value: typeof organizations) => void;
    const pendingOrganizations = new Promise<typeof organizations>((resolve) => { resolveOrganizations = resolve; });
    renderPlatform(fakeApi({ platformOrganizations: vi.fn(() => pendingOrganizations) }));
    const table = await screen.findByRole("table", { name: "Tenants" });
    resolveOrganizations(organizations);
    const alpha = await within(table).findByRole("link", { name: "Alpha" });
    const rows = within(table).getAllByRole("row");
    fireEvent.click(alpha);
    const form = (await screen.findByRole("button", { name: "Start support access" })).closest("form")!;
    expect(form.getAttribute("method")).toBe("post");
    expect(form.getAttribute("action")).toBe("/support/start");
    expect((form.querySelector('input[name="organization"]') as HTMLInputElement).value).toBe("org_alpha");
    expect((form.querySelector('input[name="csrf"]') as HTMLInputElement).value).toBe("staff-csrf");
    const reason = within(form).getByLabelText("Support reason for Alpha") as HTMLSelectElement;
    expect(reason.required).toBe(true);
    expect(reason.value).toBe("");
    expect(Array.from(reason.options).map((option) => option.value)).toEqual(["", "customer-request", "account-recovery", "troubleshooting"]);
    expect(within(rows[2]!).queryByRole("button", { name: "Start support access" })).toBeNull();

  });

  it("shows restricted access with individual rows and filters both allowlists live", async () => {
    const api = fakeApi({ platformAllowlist: vi.fn(async () => ({ ...allowlist,
      allowed_domains: ["example.org", "detent.build"],
      allowed_emails: ["dana@example.test", "cory@detent.build", "operator@example.test"],
    })) });
    renderPlatform(api, "/platform/allowlist");
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
    expect(within(domains).getAllByRole("row").map((row) => row.textContent)).toEqual(["example.org", "detent.build"]);
    expect(within(emails).getAllByRole("row").map((row) => row.textContent)).toEqual(["dana@example.test", "cory@detent.build", "operator@example.test"]);
    const filter = within(panel).getByRole("searchbox", { name: "Filter domains and emails" });
    fireEvent.change(filter, { target: { value: " DETENT " } });
    expect(within(domains).getAllByRole("row").map((row) => row.textContent)).toEqual(["detent.build"]);
    expect(within(emails).getAllByRole("row").map((row) => row.textContent)).toEqual(["cory@detent.build"]);
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
    renderPlatform(fakeApi({ platformAllowlist: vi.fn(async () => ({ ...allowlist, self_service, open,
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
    renderPlatform(fakeApi({ platformAllowlist: vi.fn(async () => ({ ...allowlist, open: undefined,
      allowed_domains: [], allowed_emails: [], [list]: allowlist[list],
    })) }), "/platform/allowlist");
    const panel = await screen.findByRole("region", { name: "Sign-up allowlist" });
    expect(within(panel).getByText("Restricted")).toBeTruthy();
    expect(within(panel).getByText(allowlist[list][0]!)).toBeTruthy();
    expect(within(panel).getByText(list === "allowed_domains" ? "No individual emails configured." : "No domains configured.")).toBeTruthy();
  });

  it("shows registry, tenant and admission health", async () => {
    renderPlatform(fakeApi(), "/platform/health");
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
    renderPlatform(fakeApi({ platformHealth: vi.fn(async () => value) }), "/platform/health");
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
    const view = renderPlatform(api, "/platform/health");
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
    renderPlatform(fakeApi({ platformOrganizations: refused }));
    expect((await screen.findByRole("alert")).textContent).toContain("limited to Detent staff");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it.each([
    ["session", "/platform/tenants"],
    ["organizations", "/platform/tenants"],
    ["platformOrganizations", "/platform/tenants"],
    ["platformHealth", "/platform/health"],
    ["platformAllowlist", "/platform/allowlist"],
    ["platformTenant", "/platform/tenants?tenant=org_alpha"],
  ] as const)("sends an expired %s session back to sign-in", async (method, path) => {
    const expired = vi.fn(async () => {
      throw new AccountError({ status: 401, code: "unauthenticated", message: "Sign in" });
    });
    renderPlatform(fakeApi({ platformOrganizations: vi.fn(async () => administrator), [method]: expired }), path);
    await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM));
  });

  it.each(["alp", "org_alpha", "dana@"])('finds a tenant by "%s" and opens its addressable detail', async (query) => {
    const user = userEvent.setup();
    const router = renderPlatform(fakeApi());
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
    const router = renderPlatform(api);
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
    renderPlatform(fakeApi({ platformHealth: vi.fn(async () => health) }));
    const nav = await screen.findByRole("navigation", { name: "Platform navigation" });
    expect(await within(nav).findByText(label)).toBeTruthy();
    expect(within(nav).getByText(label).getAttribute("data-slot")).toBe("badge");
    expect(await within(nav).findByText("2")).toBeTruthy();
    expect(within(nav).getByText("2").getAttribute("data-slot")).toBe("badge");
  });

  it("keeps the shell for a non-staff account without calling platform APIs", async () => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "customer@example.test", csrf: "c", platform_role: "" })) });
    renderPlatform(api);
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
  it("is hidden from staff who are not entitlement administrators", async () => {
    const api = fakeApi();
    renderPlatform(api);
    await screen.findByRole("table", { name: "Tenants" });
    expect(screen.queryByRole("region", { name: "Complimentary plans" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Grant complimentary plan" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Grant model choice" })).toBeNull();
    expect(api.platformTenant).not.toHaveBeenCalled();
  });

  it.each(["admin", "billing"])("shows base, effective plan and active grants of ready organizations to %s", async (role) => {
    const api = fakeApi({
      session: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", platform_role: role })),
      platformOrganizations: vi.fn(async () => administrator),
    });
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    await within(plan).findByText("pilot_free v1 (base) + 1 complimentary grant");
    expect(within(plan).getByText("pilot_free v1")).toBeTruthy();
    const grants = within(plan).getByRole("table", { name: "Active grants for Alpha" });
    expect(within(grants).getByText("design partner")).toBeTruthy();
    expect(within(grants).getByText("plans@detent.build")).toBeTruthy();
    expect(within(grants).getByText("2026-12-31")).toBeTruthy();
    expect(within(grants).getByText("hosted_artifacts, projects")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Plan for Beta" })).toBeNull();
    expect(api.platformEntitlements).toHaveBeenCalledWith("org_alpha");
  });

  it("requires a reason and posts the grant with the revision and an idempotency key", async () => {
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator) });
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    const picker = within(dialog).getByLabelText("Plan") as HTMLSelectElement;
    expect(Array.from(picker.options).map((option) => option.textContent)).toEqual(["comp_team v1"]);
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
    await waitFor(() => expect(api.platformEntitlements).toHaveBeenCalledTimes(2));
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
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant model choice" }));
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
    await within(plan).findByText("Model choice: Enabled");
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
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
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
        if (reads > 1) throw new AccountError({ status: 503, code: "tenant_unavailable", message: "The organization's Hub is unavailable" });
        return entitlements(organization);
      }),
    });
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
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
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "design partner" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(STALE_PLAN_MESSAGE);
    await waitFor(() => expect(api.platformEntitlements).toHaveBeenCalledTimes(2));
  });

  it("revokes a grant only with a reason", async () => {
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator) });
    renderPlatform(api);
    fireEvent.click(await screen.findByRole("link", { name: "Alpha" }));
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Revoke" }));
    const dialog = await screen.findByRole("dialog", { name: /^(Grant |Revoke )/ });
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
    renderPlatform(fakeApi({ platformOrganizations: vi.fn(async () => ({ ...organizations, organizations: values })) }));
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
    for (const query of ["DANA", "org_alpha", "alpha"]) {
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
    renderPlatform(fakeApi());
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

  it.each(["viewer", "support", "billing", "admin"])("opens an addressable page with role-gated actions for %s", async (role) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "staff@example.test", csrf: "staff-csrf", platform_role: role })) });
    renderPlatform(api, "/platform/tenants?tenant=org_alpha");
    const sheet = await screen.findByRole("region", { name: "Alpha" });
    await within(sheet).findByRole("region", { name: "Members (1)" });
    expect(within(sheet).getByRole("region", { name: "Members (1)" }).textContent).toContain("owner");
    expect(within(sheet).getByRole("region", { name: "Runners (1)" }).textContent).toContain("Mac Studio online");
    expect(within(sheet).getByRole("region", { name: "Projects (1)" }).textContent).toContain("Detent");
    expect(within(sheet).getByRole("region", { name: "Provisioning timeline" }).textContent).toContain("requested");
    expect(within(sheet).queryByRole("button", { name: "Start support access" }) !== null).toBe(role === "support" || role === "admin");
    for (const name of ["Grant complimentary plan", "Grant model choice", "Revoke"]) {
      expect(within(sheet).queryByRole("button", { name }) !== null).toBe(role === "billing" || role === "admin");
    }
    expect(within(sheet).queryByRole("button", { name: "Resume provisioning" })).toBeNull();
  });

  it("renders unavailable sections independently and confirms failed tenant resume", async () => {
    const api = fakeApi();
    const original = api.platformTenant;
    api.platformTenant = vi.fn(async (id) => ({ ...await original(id), members: null, projects: null }));
    renderPlatform(api, "/platform/tenants?tenant=org_beta");
    const sheet = await screen.findByRole("region", { name: "Beta" });
    expect((await within(sheet).findByRole("region", { name: "Members" })).textContent).toContain("Unavailable");
    expect(within(sheet).getByRole("region", { name: "Projects" }).textContent).toContain("Unavailable");
    expect(within(sheet).getByRole("region", { name: "Runners (1)" })).toBeTruthy();
    fireEvent.click(within(sheet).getByRole("button", { name: "Resume provisioning" }));
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

function openStaff(overrides: Partial<EntryApi> = {}) {
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
  const router = makeEntryRouter(createMemoryHistory({ initialEntries: ["/platform/staff"] }));
  render(<EntryApiContext.Provider value={api}><RouterProvider router={router} /></EntryApiContext.Provider>);
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
    const api = openStaff();
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
    const api = openStaff();
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
    openStaff({ platformMembers: vi.fn(async () => value) });
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
    const api = openStaff({ changePlatformMember: vi.fn(async () => { throw new AccountError({ status: 409, code: "already_member", message: "Already a member" }); }) });
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
    const api = openStaff({ platformMembers: vi.fn(async () => current), changePlatformMember: vi.fn(async () => {
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
    openStaff({ changePlatformMember: write });
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
      actor: "admin@detent.build",
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
    const router = renderPlatform(
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
    const router = renderPlatform(
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
    renderPlatform(api, "/platform/audit?actor=admin");
    const table = await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    expect(
      within(table).getByRole("link", { name: "Alpha" }).getAttribute("href"),
    ).toBe("/platform/tenants?tenant=org_alpha");
    expect(
      within(table)
        .getByRole("link", { name: "Beta" })
        .getAttribute("href"),
    ).toBe("/platform/tenants?tenant=org_beta");
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
    renderPlatform(api, "/platform/audit");
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
      renderPlatform(
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
    const router = renderPlatform(api, "/platform/audit?tenant=org_alpha&event=support_started");
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
    renderPlatform(api, "/platform/audit?tenant=org_alpha&actor=admin&event=plan_granted&from=2026-09-01&to=2026-10-01");
    const table = await screen.findByRole("table", { name: "Audit events" }, { timeout: 5000 });
    expect(table.textContent).toContain("Plan granted");
    expect(table.textContent).toContain("Growth (version 1) until Dec 31, 2026");
    expect(table.textContent).toContain('customer, "pilot"');
    expect(table.textContent).not.toContain("plan_granted");
    fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
    await waitFor(() => expect(click).toHaveBeenCalledTimes(1));
    const csv = await exported.text();
    expect(csv).toBe('"When (UTC)","Action","Tenant","By","Details","Reason"\r\n' +
      '"2026-10-01T12:00:00Z","Plan granted","Alpha","admin@detent.build","Growth (version 1) until Dec 31, 2026","customer, ""pilot"""\r\n' +
      '"2026-09-30T10:00:00Z","Plan granted","Alpha","admin@detent.build","Growth (version 1)","\'=customer reason"\r\n');
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
    renderPlatform(api, "/platform/audit?actor=admin");
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
    renderPlatform(api, "/platform/audit?actor=admin");
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
    renderPlatform(fakeApi(), "/platform/audit?actor=nobody");
    await screen.findByText("No events match these filters.");
    expect(screen.queryByRole("table")).toBeNull();
  });
});
