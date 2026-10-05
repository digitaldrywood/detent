// @vitest-environment jsdom
//
// The shared entry's platform console: staff land there instead of the
// organization chooser, and the console renders organizations with their
// support-access form, the signup allowlist, and service health.
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import { type EntryApi, SIGN_IN_PLATFORM } from "../../src/app/entry/api.ts";
import {
  CreateOrganization,
  EntryApiContext,
  OrganizationChooser,
} from "../../src/app/entry/EntryScreens.tsx";
import { formatBytes, PlatformConsole } from "../../src/app/entry/PlatformConsole.tsx";
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
  return {
    organizations: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", organizations: [], pending: [], can_create: false, staff: true })),
    session: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", can_create: false, staff: true })),
    provisioning: vi.fn(async () => ({ id: "", name: "", state: "", step: "", error: "", can_resume: false })),
    createOrganization: vi.fn(async () => ({ next: "/" })),
    resume: vi.fn(async () => ({ next: "/" })),
    platformOrganizations: vi.fn(async () => organizations),
    platformAllowlist: vi.fn(async () => allowlist),
    platformHealth: vi.fn(async () => health),
    platformEntitlements: vi.fn(async () => entitlements),
    changePlatformEntitlement: vi.fn(async () => ({ action: "grant", grant_id: "comp_new" })),
    ...overrides,
  };
}

function renderWith(api: EntryApi, element: React.ReactElement) {
  return render(<EntryApiContext.Provider value={api}>{element}</EntryApiContext.Provider>);
}

describe("staff landing", () => {
  it.each([
    ["chooser", OrganizationChooser],
    ["create", CreateOrganization],
  ] as const)("sends staff from the %s screen to the platform console without customer actions", async (_, Screen) => {
    const navigate = vi.fn();
    renderWith(fakeApi(), <Screen onNavigate={navigate} />);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/platform"));
    expect(screen.queryByRole("button", { name: /Create organization/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Join/ })).toBeNull();
  });

  it("keeps customers on the chooser", async () => {
    const navigate = vi.fn();
    const api = fakeApi({
      organizations: vi.fn(async () => ({ email: "dana@example.test", csrf: "c", organizations: [], pending: [], can_create: true, staff: false })),
    });
    renderWith(api, <OrganizationChooser onNavigate={navigate} />);
    expect(await screen.findByRole("button", { name: "Create organization" })).toBeTruthy();
    expect(navigate).not.toHaveBeenCalledWith("/platform");
  });
});

describe("platform console", () => {
  it("lists organizations with state, last error, owner and billing", async () => {
    renderWith(fakeApi(), <PlatformConsole />);
    const table = await screen.findByRole("table", { name: "Organizations" });
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
    renderWith(fakeApi(), <PlatformConsole />);
    const table = await screen.findByRole("table", { name: "Organizations" });
    const rows = within(table).getAllByRole("row");
    const form = within(rows[1]!).getByRole("button", { name: "Start support access" }).closest("form")!;
    expect(form.getAttribute("method")).toBe("post");
    expect(form.getAttribute("action")).toBe("/support/start");
    expect((form.querySelector('input[name="organization"]') as HTMLInputElement).value).toBe("org_alpha");
    expect((form.querySelector('input[name="csrf"]') as HTMLInputElement).value).toBe("staff-csrf");
    const reason = within(form).getByLabelText("Support reason for Alpha") as HTMLSelectElement;
    expect(reason.required).toBe(true);
    expect(reason.value).toBe("");
    expect(Array.from(reason.options).map((option) => option.value)).toEqual(["", "customer-request", "account-recovery", "troubleshooting"]);
    expect(within(rows[2]!).queryByRole("button", { name: "Start support access" })).toBeNull();
    expect(within(rows[2]!).getByText("Not ready")).toBeTruthy();
  });

  it("shows the allowlist read-only with its configuration source", async () => {
    renderWith(fakeApi(), <PlatformConsole />);
    const panel = await screen.findByRole("region", { name: "Signup allowlist" });
    expect(within(panel).getByText("dana@example.test")).toBeTruthy();
    expect(within(panel).getByText("example.org")).toBeTruthy();
    expect(within(panel).getByText(/\/etc\/detent\/cloud\.yaml under allocation\.allowed_emails and allocation\.allowed_domains/)).toBeTruthy();
    expect(within(panel).queryByRole("textbox")).toBeNull();
  });

  it("shows registry, tenant and admission health", async () => {
    renderWith(fakeApi(), <PlatformConsole />);
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
    renderWith(fakeApi({ platformOrganizations: refused, platformAllowlist: refused, platformHealth: refused }), <PlatformConsole />);
    expect((await screen.findByRole("alert")).textContent).toContain("limited to Detent staff");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("sends an expired session back to sign-in", async () => {
    const expired = vi.fn(async () => {
      throw new AccountError({ status: 401, code: "unauthenticated", message: "Sign in" });
    });
    renderWith(fakeApi({ platformOrganizations: expired }), <PlatformConsole />);
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
    renderWith(api, <PlatformConsole />);
    await screen.findByRole("table", { name: "Organizations" });
    expect(screen.queryByRole("region", { name: "Complimentary plans" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Grant complimentary plan" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Grant model choice" })).toBeNull();
    expect(api.platformEntitlements).not.toHaveBeenCalled();
  });

  it("shows base, effective plan and active grants of ready organizations to administrators", async () => {
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator) });
    renderWith(api, <PlatformConsole />);
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
    renderWith(api, <PlatformConsole />);
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
    const dialog = await screen.findByRole("dialog");
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
    renderWith(api, <PlatformConsole />);
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant model choice" }));
    const dialog = await screen.findByRole("dialog");
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
    renderWith(api, <PlatformConsole />);
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
    const dialog = await screen.findByRole("dialog");
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
    renderWith(api, <PlatformConsole />);
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "design partner" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect((await within(plan).findByRole("alert")).textContent).toContain("may be out of date");
  });

  it("shows the stale revision message and reloads the plan", async () => {
    const stale = vi.fn(async () => {
      throw new AccountError({ status: 409, code: "revision_conflict", message: "Resource has changed" });
    });
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator), changePlatformEntitlement: stale });
    renderWith(api, <PlatformConsole />);
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Grant complimentary plan" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "design partner" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Grant" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(STALE_PLAN_MESSAGE);
    await waitFor(() => expect(api.platformEntitlements).toHaveBeenCalledTimes(2));
  });

  it("revokes a grant only with a reason", async () => {
    const api = fakeApi({ platformOrganizations: vi.fn(async () => administrator) });
    renderWith(api, <PlatformConsole />);
    const plan = await screen.findByRole("region", { name: "Plan for Alpha" });
    fireEvent.click(await within(plan).findByRole("button", { name: "Revoke" }));
    const dialog = await screen.findByRole("dialog");
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
