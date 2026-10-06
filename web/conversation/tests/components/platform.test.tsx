// @vitest-environment jsdom
//
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import { type EntryApi, SIGN_IN_PLATFORM } from "../../src/app/entry/api.ts";
import {
  EntryApiContext,
} from "../../src/app/entry/EntryScreens.tsx";
import { filterTenants } from "../../src/app/entry/PlatformTenants.tsx";
import { formatBytes } from "../../src/app/entry/PlatformConsole.tsx";
import { makeEntryRouter } from "../../src/app/entry/router.tsx";
import { STALE_PLAN_MESSAGE } from "../../src/app/entry/ComplimentaryPlans.tsx";

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
    platformOrganizations: vi.fn(async () => organizations),
    platformAllowlist: vi.fn(async () => allowlist),
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
  it.each(["admin", "support", "billing", "viewer"])("shows the shell and readable navigation for %s", async (role) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: "member@example.test", csrf: "c", platform_role: role })) });
    renderPlatform(api);
    const nav = await screen.findByRole("navigation", { name: "Platform navigation" });
    await screen.findByRole("heading", { name: "Tenants", level: 1 });
    expect(within(nav).getAllByRole("link").map((link) => link.textContent)).toEqual(
      role === "admin" ? ["Tenants", "Staff", "Audit", "Health", "Allowlist"] : ["Tenants", "Audit", "Health", "Allowlist"],
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
      organizations: [{ id: "org_alpha", name: "My Alpha", url: "/organizations/org_alpha/organization" }],
    })) });
    const router = renderPlatform(api);
    fireEvent.click(await screen.findByRole("button", { name: "Open organization" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "My Alpha" }).getAttribute("href")).toBe("/organizations/org_alpha/organization");
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
  it("filters name, id and creator and combines state and plan filters with sorting", () => {
    const values = organizations.organizations.map((tenant, index) => ({ ...tenant, plan: index === 0 ? "growth" : "free" }));
    expect(filterTenants(values, "DANA", "all", "all", "created_at").map((tenant) => tenant.id)).toEqual(["org_alpha"]);
    expect(filterTenants(values, "org_beta", "all", "all", "created_at").map((tenant) => tenant.name)).toEqual(["Beta"]);
    expect(filterTenants(values, "Alpha", "failed", "all", "name")).toEqual([]);
    expect(filterTenants(values, "", "ready", "growth", "name")).toHaveLength(1);
    expect(filterTenants(values, "", "ready", "free", "name")).toEqual([]);
    for (const sort of ["name", "state", "created_at"] as const) {
      expect(filterTenants(values, "", "all", "all", sort, true)).toEqual(filterTenants(values, "", "all", "all", sort).reverse());
    }
  });

  it("updates counts, empty results, unavailable cells and sortable headers", async () => {
    renderPlatform(fakeApi());
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(within(table).getAllByText("—")).toHaveLength(6);
    expect(screen.getByText("2 tenants · 1 ready")).toBeTruthy();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "eve@" } });
    expect(screen.getByText("1 tenants · 0 ready")).toBeTruthy();
    expect(within(table).queryByRole("button", { name: "Alpha" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "missing" } });
    expect(screen.getByText("No tenants match")).toBeTruthy();
    fireEvent.change(screen.getByRole("textbox", { name: "Search tenants" }), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Tenant" }));
    fireEvent.click(screen.getByRole("button", { name: "Tenant" }));
    expect(screen.getByRole("columnheader", { name: /Tenant/ }).getAttribute("aria-sort")).toBe("descending");
    expect(within(screen.getAllByRole("row")[1]!).getByRole("button", { name: "Beta" })).toBeTruthy();
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
