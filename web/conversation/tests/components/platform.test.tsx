// @vitest-environment jsdom
//
// The shared entry's platform console: staff land there instead of the
// organization chooser, and the console renders organizations with their
// support-access form, the signup allowlist, and service health.
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import { type EntryApi, SIGN_IN_PLATFORM } from "../../src/app/entry/api.ts";
import {
  CreateOrganization,
  EntryApiContext,
  JoinInvitation,
  OrganizationChooser,
} from "../../src/app/entry/EntryScreens.tsx";
import { formatBytes, PlatformConsole } from "../../src/app/entry/PlatformConsole.tsx";

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
      owner_email: "dana@example.test", created_at: "2026-09-20T10:00:00Z", updated_at: "2026-09-20T10:05:00Z",
      billing: { available: true, status: "active" }, can_support: true, plan: null, grants: null, member_count: null, runner_count: null,
    },
    {
      id: "org_beta", name: "Beta", state: "failed", step: "tenant_files", attempts: 3, error_code: "tenant_start_failed", managed: true,
      owner_email: "eve@example.test", created_at: "2026-09-21T10:00:00Z", updated_at: "2026-09-21T10:05:00Z",
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

function fakeApi(overrides: Partial<EntryApi> = {}): EntryApi {
  return {
    organizations: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", organizations: [], pending: [], can_create: false, staff: true })),
    session: vi.fn(async () => ({ email: "admin@detent.build", csrf: "staff-csrf", can_create: false, staff: true })),
    provisioning: vi.fn(async () => ({ id: "", name: "", state: "", step: "", error: "", can_resume: false })),
    createOrganization: vi.fn(async () => ({ next: "/" })),
    resume: vi.fn(async () => ({ next: "/" })),
    joinInvitation: vi.fn(async () => ({ next: "/" })),
    platformOrganizations: vi.fn(async () => organizations),
    platformAllowlist: vi.fn(async () => allowlist),
    platformHealth: vi.fn(async () => health),
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
    ["join", JoinInvitation],
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
    expect(within(rows[2]!).getByText(/Last error: tenant_start_failed after tenant_files/)).toBeTruthy();
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
