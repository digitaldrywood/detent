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
import { formatBytes } from "../../src/app/entry/PlatformConsole.tsx";
import { makeEntryRouter } from "../../src/app/entry/router.tsx";
import { STALE_PLAN_MESSAGE } from "../../src/app/entry/ComplimentaryPlans.tsx";
import accountResults from "../../../../tests/visual/platform-accounts-data.json";

const assign = vi.fn();

beforeEach(() => {
  assign.mockReset();
  vi.stubGlobal("location", { assign, search: "", pathname: "/" });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
    platformAccounts: vi.fn(async () => ({ accounts: [], unsearched: [] })),
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
  it("searches accounts only on submit and renders memberships, invitations and incomplete tenants", async () => {
    let resolve: ((value: typeof accountResults) => void) | undefined;
    const search = vi.fn(() => new Promise<typeof accountResults>((done) => { resolve = done; }));
    renderPlatform(fakeApi({ platformAccounts: search }), "/platform/accounts");
    const input = await screen.findByRole("searchbox", { name: "Search by email" });
    const button = screen.getByRole("button", { name: "Search" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    fireEvent.change(input, { target: { value: "ab" } });
    fireEvent.submit(input.closest("form")!);
    expect(search).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: " example.test " } });
    expect(button.disabled).toBe(false);
    expect(search).not.toHaveBeenCalled();
    fireEvent.submit(input.closest("form")!);
    expect(search).toHaveBeenCalledWith("example.test");
    expect(screen.getByRole("status", { name: "Searching accounts" })).toBeTruthy();
    expect((screen.getByRole("button", { name: "Searching…" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(input.closest("form")!);
    expect(search).toHaveBeenCalledTimes(1);
    resolve!(accountResults);
    const account = await screen.findByRole("region", { name: "michael@example.test" });
    expect(account.textContent).toContain("user_01HX_michael");
    expect(account.textContent).toContain("2026-10-06 14:02Z");
    expect(within(account).getByText("support")).toBeTruthy();
    const rows = within(account).getAllByRole("row");
    expect(rows.map((row) => row.textContent)).toEqual([
      "OrganizationRoleJoinedTenant", "Parableowner2026-09-28Open tenant →", "Drywood Creekmember2026-10-01Open tenant →", "Example Coinvitedexpires 2026-10-13Open tenant →",
    ]);
    expect(within(account).getAllByRole("link").map((link) => link.getAttribute("href"))).toEqual([
      "/platform/tenants?tenant=org_parable", "/platform/tenants?tenant=org_drywood", "/platform/tenants?tenant=org_example",
    ]);
    expect(screen.getByText("No organization membership")).toBeTruthy();
    expect(screen.getByText("Not searched: Slow Tenant")).toBeTruthy();
    fireEvent.change(input, { target: { value: "missing" } });
    fireEvent.submit(input.closest("form")!);
    expect(screen.queryByRole("region", { name: "michael@example.test" })).toBeNull();
    resolve!({ accounts: [], unsearched: [] });
    expect(await screen.findByText("No accounts match")).toBeTruthy();
  });

  it.each([401, 403, 503])("handles account search failure %s", async (status) => {
    const api = fakeApi({ platformAccounts: vi.fn(async () => { throw new AccountError({ status, code: "refused", message: "Search refused" }); }) });
    renderPlatform(api, "/platform/accounts");
    const input = await screen.findByRole("searchbox");
    fireEvent.change(input, { target: { value: "member" } });
    fireEvent.submit(input.closest("form")!);
    expect((await screen.findByRole("alert")).textContent).toContain("Search refused");
    expect(screen.queryByText("No accounts match")).toBeNull();
    if (status === 401) expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM);
    expect((screen.getByRole("button", { name: "Search" }) as HTMLButtonElement).disabled).toBe(false);
  });
  it.each(["admin", "support", "billing", "viewer"])("shows the shell and readable navigation for %s", async (role) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "member@example.test", csrf: "c", platform_role: role })) });
    renderPlatform(api);
    const nav = await screen.findByRole("navigation", { name: "Platform navigation" });
    await screen.findByRole("heading", { name: "Tenants", level: 1 });
    expect(within(nav).getAllByRole("link").map((link) => link.textContent)).toEqual(
      role === "admin" ? ["Tenants", "Accounts", "Staff", "Audit", "Health", "Allowlist"] : ["Tenants", "Accounts", "Audit", "Health", "Allowlist"],
    );
    expect(within(nav).getByRole("link", { name: "Tenants" }).getAttribute("data-active")).toBe("true");
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(document.querySelector('[data-slot="sidebar-footer"]')?.textContent).toBe(role[0]!.toUpperCase() + role.slice(1));
    expect(screen.queryByRole("button", { name: "Open organization" })).toBeNull();
    expect(api.platformHealth).not.toHaveBeenCalled();
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
    fireEvent.click(await screen.findByRole("button", { name: "Open organization" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "My Alpha" }).getAttribute("href")).toBe("/organizations/org_alpha/organization");
    expect(within(menu).getByRole("menuitem", { name: "My Beta" }).getAttribute("href")).toBe("/organizations/org_beta/work");
    fireEvent.keyDown(menu, { key: "Escape" });
    fireEvent.click(screen.getByRole("link", { name: "Health" }));
    await screen.findByRole("region", { name: "Service health" });
    expect(router.state.location.pathname).toBe("/platform/health");
    expect(document.title).toBe("Health · Platform · Detent");
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(api.session).toHaveBeenCalledTimes(1);
    expect(api.organizations).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("table", { name: "Tenants" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Complimentary plans" })).toBeNull();
  });

  it("refuses a direct Staff route for a non-admin inside the shell", async () => {
    renderPlatform(fakeApi({ session: vi.fn(async () => ({ email: "support@example.test", csrf: "c", platform_role: "support" })) }), "/platform/staff");
    expect((await screen.findByRole("alert")).textContent).toBe("You do not have permission to do that.");
    expect(screen.getByText("Platform")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Staff" })).toBeNull();
  });

  it("lists organizations with state, last error, owner and billing", async () => {
    renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(within(rows[1]!).getByText("Alpha")).toBeTruthy();
    expect(within(rows[1]!).getByText("dana@example.test")).toBeTruthy();
    expect(within(rows[1]!).getByText("active")).toBeTruthy();
    expect(within(rows[1]!).getByText("2026-09-20")).toBeTruthy();
    expect(within(rows[2]!).getByText(/Last error: tenant_start_failed after tenant_files \(the tenant Hub exited 5 times in a row without staying up \(last: exit status 1\)\)/)).toBeTruthy();
    expect(within(rows[2]!).getByText("Unavailable")).toBeTruthy();
    expect(screen.getByText(/Not shown: plan, grants, member count, runner count/)).toBeTruthy();
  });

  it("starts support access through the entry form with a required reason and the CSRF token", async () => {
    renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    const rows = within(table).getAllByRole("row");
    fireEvent.click(within(rows[1]!).getByRole("button", { name: "Alpha" }));
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

  it("shows the allowlist read-only with its configuration source", async () => {
    renderPlatform(fakeApi(), "/platform/allowlist");
    const panel = await screen.findByRole("region", { name: "Signup allowlist" });
    expect(within(panel).getByText("dana@example.test")).toBeTruthy();
    expect(within(panel).getByText("example.org")).toBeTruthy();
    expect(within(panel).getByText(/\/etc\/detent\/cloud\.yaml under allocation\.allowed_emails and allocation\.allowed_domains/)).toBeTruthy();
    expect(within(panel).queryByRole("textbox")).toBeNull();
  });

  it("shows registry, tenant and admission health", async () => {
    renderPlatform(fakeApi(), "/platform/health");
    const panel = await screen.findByRole("region", { name: "Service health" });
    expect(within(panel).getByText("OK")).toBeTruthy();
    expect(within(panel).getByText("1 of 2")).toBeTruthy();
    expect(within(panel).getByText("2 of 20")).toBeTruthy();
    expect(within(panel).getByText("10.0 GiB (floor 2.0 GiB)")).toBeTruthy();
    expect(within(panel).getByText("Not measurable")).toBeTruthy();
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "Alpha" }));
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
    const names = () => within(table).getAllByRole("row").slice(1).map((row) => within(row).getByRole("button").textContent);
    expect(names()).toEqual(["Beta", "Alpha"]);
    for (const [header, ascending, descending] of [
      ["Tenant", ["Alpha", "Beta"], ["Beta", "Alpha"]],
      ["State", ["Beta", "Alpha"], ["Alpha", "Beta"]],
      ["Created", ["Beta", "Alpha"], ["Alpha", "Beta"]],
    ] as const) {
      fireEvent.click(within(table).getByRole("button", { name: header }));
      expect(names()).toEqual(ascending);
      expect(within(table).getByRole("columnheader", { name: header }).getAttribute("aria-sort")).toBe("ascending");
      fireEvent.click(within(table).getByRole("button", { name: header }));
      expect(names()).toEqual(descending);
      expect(within(table).getByRole("columnheader", { name: header }).getAttribute("aria-sort")).toBe("descending");
    }
    const search = screen.getByRole("textbox", { name: "Search tenants" });
    for (const query of ["DANA", "org_alpha", "alpha"]) {
      fireEvent.change(search, { target: { value: query } });
      expect(names()).toEqual(["Alpha"]);
    }
    fireEvent.change(search, { target: { value: "" } });
    await user.click(screen.getByRole("combobox", { name: "State" }));
    await user.click(await screen.findByRole("option", { name: "ready" }));
    await user.click(screen.getByRole("combobox", { name: "Plan" }));
    await user.click(await screen.findByRole("option", { name: "growth" }));
    expect(names()).toEqual(["Alpha"]);
    expect(screen.getByText("1 tenants · 1 ready")).toBeTruthy();
    await user.click(screen.getByRole("combobox", { name: "Plan" }));
    await user.click(await screen.findByRole("option", { name: "free" }));
    expect(screen.getByText("No tenants match")).toBeTruthy();
  });

  it("updates counts, empty results and unavailable cells", async () => {
    renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(within(table).getAllByText("—")).toHaveLength(6);
    expect(screen.getByText("2 tenants · 1 ready")).toBeTruthy();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "eve@" } });
    expect(screen.getByText("1 tenants · 0 ready")).toBeTruthy();
    expect(within(table).queryByRole("button", { name: "Alpha" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "missing" } });
    expect(screen.getByText("No tenants match")).toBeTruthy();

  });

  it.each(["viewer", "support", "billing", "admin"])("opens an addressable sheet with role-gated actions for %s", async (role) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "staff@example.test", csrf: "staff-csrf", platform_role: role })) });
    renderPlatform(api, "/platform/tenants?tenant=org_alpha");
    const sheet = await screen.findByRole("dialog", { name: "Alpha" });
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
    const sheet = await screen.findByRole("dialog", { name: "Beta" });
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
  added_by: bootstrap ? "bootstrap" : "admin@example.test", added_at: "2026-10-07T12:00:00Z", updated_at: "2026-10-07T12:00:00Z" });
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
    await screen.findByRole("table", { name: "Platform members" });
    fireEvent.click(screen.getByRole("button", { name: "Add member" }));
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
    const row = (await screen.findByText("new@example.test")).closest("tr")!;
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
    await screen.findByRole("table");
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
    { name: "own", value: listing, email: "admin@example.test", tooltip: "You cannot change your own role" },
    { name: "bootstrap", value: listing, email: "bootstrap@example.test", tooltip: "The bootstrap admin cannot be demoted" },
    { name: "last admin", value: { ...listing, self: { email: "reader@example.test", role: "admin" }, members: [member("last@example.test", "admin")] }, email: "last@example.test", tooltip: "At least one admin is required" },
  ])("protects the $name row", async ({ value, email, tooltip }) => {
    openStaff({ platformMembers: vi.fn(async () => value) });
    const table = await screen.findByRole("table", { name: "Platform members" });
    const row = within(table).getByRole("combobox", { name: `Role for ${email}` }).closest("tr")!;
    expect((within(row).getByRole("combobox") as HTMLButtonElement).disabled).toBe(true);
    expect(within(row).queryByRole("button", { name: "Remove" })).toBeNull();
    expect(within(row).getByLabelText(tooltip)).toBeTruthy();
  });

  it("shows a duplicate error inline and allows correction", async () => {
    const api = openStaff({ changePlatformMember: vi.fn(async () => { throw new AccountError({ status: 409, code: "already_member", message: "Already a member" }); }) });
    await screen.findByRole("table");
    fireEvent.click(screen.getByRole("button", { name: "Add member" }));
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
    await screen.findByRole("table");
    await selectRole("Role for support@example.test", "Billing");
    const dialog = await screen.findByRole("dialog");
    reason(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Change role" }));
    await screen.findByText(STALE_STAFF_MESSAGE);
    expect((screen.getByRole("button", { name: "Add member" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(api.platformMembers).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Role for support@example.test" }).textContent).toContain("Viewer"));
    expect((screen.getByRole("button", { name: "Add member" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("retries an unconfirmed write with the same key and input", async () => {
    const write = vi.fn(async () => { throw new AccountError({ status: 503, code: "unavailable", message: "Retry to verify" }); });
    openStaff({ changePlatformMember: write });
    await screen.findByRole("table");
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
    await screen.findByRole("table", { name: "Audit events" });
    expect(document.title).toBe("Audit · Platform · Detent");
    expect((screen.getByLabelText("Actor") as HTMLInputElement).value).toBe(
      "admin",
    );
    await waitFor(() =>
      expect((screen.getByLabelText("Tenant") as HTMLInputElement).value).toBe(
        "Alpha",
      ),
    );
    expect(screen.getByLabelText("Event").textContent).toBe("support_started");
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe(
      "2026-09-01",
    );
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe(
      "2026-10-01",
    );
    expect(api.platformAudit).toHaveBeenCalledWith(
      "tenant=org_alpha&actor=admin&event=support_started&from=2026-09-01&to=2026-10-01",
    );
    fireEvent.change(screen.getByLabelText("Actor"), {
      target: { value: "system" },
    });
    await waitFor(() =>
      expect(router.state.location.searchStr).toContain("actor=system"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(router.state.location.searchStr).toBe(""));
    expect((screen.getByLabelText("Actor") as HTMLInputElement).value).toBe("");
    await act(async () => {
      router.history.back();
    });
    await waitFor(() =>
      expect((screen.getByLabelText("Actor") as HTMLInputElement).value).toBe(
        "system",
      ),
    );
    await act(async () => {
      router.history.forward();
    });
    await waitFor(() =>
      expect((screen.getByLabelText("Actor") as HTMLInputElement).value).toBe(
        "",
      ),
    );
  });

  it("uses searchable tenant and stored event choices in the URL", async () => {
    const router = renderPlatform(
      fakeApi({ platformAudit: vi.fn(async () => audit) }),
      "/platform/audit",
    );
    await screen.findByRole("table", { name: "Audit events" });
    fireEvent.click(screen.getByLabelText("Event"));
    await userEvent.click(
      await screen.findByRole("option", { name: "organization_ready" }),
    );
    await waitFor(() =>
      expect(router.state.location.searchStr).toBe("?event=organization_ready"),
    );
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
    const table = await screen.findByRole("table", { name: "Audit events" });
    expect(
      within(table).getByRole("link", { name: "Alpha" }).getAttribute("href"),
    ).toBe("/platform/tenants?tenant=org_alpha");
    expect(
      within(table)
        .getByRole("link", { name: "org_beta" })
        .getAttribute("href"),
    ).toBe("/platform/tenants?tenant=org_beta");
    expect(within(table).getByText("deleted")).toBeTruthy();
    expect(api.platformAudit).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    await screen.findByText("older reason");
    expect(within(table).getAllByRole("row")).toHaveLength(4);
    expect(within(table).getByText("unresolved_subject").className).toBe(
      "font-mono",
    );
    expect(api.platformAudit).toHaveBeenLastCalledWith(
      "actor=admin&cursor=older-cursor",
    );
    expect(api.platformAudit).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: "Load older" })).toBeNull();
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
    await screen.findByRole("table", { name: "Audit events" });
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    expect(
      screen
        .getByRole("button", { name: "Loading older…" })
        .hasAttribute("disabled"),
    ).toBe(true);
    fireEvent.change(screen.getByLabelText("Actor"), {
      target: { value: "system" },
    });
    await waitFor(() =>
      expect(api.platformAudit).toHaveBeenCalledWith("actor=system"),
    );
    await screen.findByText("generation 1");
    await act(async () => {
      resolveOlder({
        ...audit,
        rows: [{ ...audit.rows[0]!, detail: "stale older row" }],
      });
    });
    expect(screen.queryByText("stale older row")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Audit unavailable",
    );
    expect(screen.getByText("generation 1")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    await screen.findByText("troubleshooting");
    expect(screen.getByText("generation 1")).toBeTruthy();
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
      if (status === 401) expect(assign).toHaveBeenCalledWith(SIGN_IN_PLATFORM);
    },
  );

  it("shows an empty state for filters with no matches", async () => {
    renderPlatform(fakeApi(), "/platform/audit?actor=nobody");
    await screen.findByText("No events match these filters.");
    expect(screen.queryByRole("table")).toBeNull();
  });
});
