// @vitest-environment jsdom
//
// The shared entry's chooser, creation and provisioning screens, each against
// a scripted entry API, plus the entry router.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import {
  CHOOSE_ORGANIZATION,
  currentStepIndex,
  type EntryApi,
  makeEntryApi,
  PROVISIONING_STEPS,
  SIGN_IN_ORGANIZATIONS,
} from "../../src/app/entry/api.ts";
import {
  CreateOrganization,
  EntryApiContext,
  OrganizationChooser,
  ProvisioningProgress,
} from "../../src/app/entry/EntryScreens.tsx";
import { ENTRY_ROUTE_PATHS, isEntrySurface, makeEntryRouter } from "../../src/app/entry/router.tsx";

if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const assign = vi.fn();
const replace = vi.fn();

beforeEach(() => {
  assign.mockReset();
  replace.mockReset();
  vi.stubGlobal("location", { assign, replace, search: "", pathname: "/" });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const listing = {
  email: "member@example.test",
  csrf: "csrf-token",
  organizations: [{ id: "org_a", name: "Alpha", url: "/organizations/org_a/organization" }],
  pending: [{ id: "org_b", name: "Beta", url: "/organizations/org_b/provisioning", state: "failed" }],
  can_create: true,
};

function fakeApi(overrides: Partial<EntryApi> = {}): EntryApi {
  return {
    organizations: vi.fn(async () => listing),
    session: vi.fn(async () => ({ email: listing.email, csrf: listing.csrf, can_create: true })),
    provisioning: vi.fn(async () => ({ id: "org_b", name: "Beta", state: "allocating", step: "admission", error: "", can_resume: false })),
    createOrganization: vi.fn(async () => ({ next: "/organizations/org_new/provisioning" })),
    resume: vi.fn(async () => ({ next: "/organizations/org_b/provisioning" })),
    platformTenant: vi.fn(async () => { throw new Error("Tenant not requested"); }),
    adjustPlatformAICredits: vi.fn(async () => ({ organization_id: "org_alpha", balance_micros: 0 })),
    resumePlatformTenant: vi.fn(async () => ({ next: "/platform/tenants" })),
    platformUsers: vi.fn(async () => ({ users: [], unsearched: [], organizations: [], total: 0, total_users: 0, page: 1, page_size: 50, counts: { all: 0, staff: 0, no_organization: 0, invited: 0 } })),
    platformOrganizations: vi.fn(async () => ({ email: "", csrf: "", can_support: false, organizations: [], unavailable: [] })),
    platformAllowlist: vi.fn(async () => ({ self_service: false, allowed_emails: [], allowed_domains: [], source: { file: "", keys: [] } })),
    platformMembers: vi.fn(async () => ({ members: [], revision: 1, self: { email: "admin@example.com", role: "admin" } })),
    changePlatformMember: vi.fn(async () => ({ email: "new@example.test", role: "viewer", revision: 2 })),
    platformAudit: vi.fn(async () => ({ rows: [], events: [], tenants: [], next_cursor: "" })),
    platformHealth: vi.fn(async () => ({ registry: { ok: true } })),
    platformEntitlements: vi.fn(async () => ({
      organization_id: "", base: { id: "", version: 1 }, effective_base: { id: "", version: 1 }, source: "base", revision: 1, grants: [], plans: [],
    })),
    changePlatformEntitlement: vi.fn(async () => ({ action: "grant", grant_id: "" })),
    ...overrides,
  };
}

function renderWith(api: EntryApi, element: React.ReactElement) {
  return render(<EntryApiContext.Provider value={api}>{element}</EntryApiContext.Provider>);
}

describe("organization chooser", () => {
  it("replaces the organization monogram with its authenticated logo, falling back when absent", async () => {
    renderWith(fakeApi({ organizations: vi.fn(async () => ({ ...listing, organizations: [{ ...listing.organizations[0]!, logo_url: "/organizations/org_a/api/v2/logo-image" }] })) }), <OrganizationChooser onNavigate={vi.fn()} />);
    const logo = await screen.findByRole("img", { name: "Alpha logo" });
    expect(logo.getAttribute("src")).toBe("/organizations/org_a/api/v2/logo-image");
    fireEvent.error(logo);
    expect(screen.queryByRole("img", { name: "Alpha logo" })).toBeNull();
    expect(screen.getByRole("link", { name: "Alpha" }).textContent).toContain("A");
  });

  it("lists organizations as links, pending ones with their state, and the actions", async () => {
    const navigate = vi.fn();
    renderWith(fakeApi(), <OrganizationChooser onNavigate={navigate} />);
    const link = await screen.findByRole("link", { name: "Alpha" });
    expect(link.getAttribute("href")).toBe("/organizations/org_a/organization");
    expect(screen.getByText("Needs attention")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Beta" }).getAttribute("href")).toBe("/organizations/org_b/provisioning");
    expect(screen.getByRole("link", { name: "Create organization" }).getAttribute("href")).toBe("/organizations/new");
    expect(screen.getByText("You belong to 1 organization. 1 is being set up.")).toBeTruthy();
    expect(screen.getByText("Signed in as member@example.test")).toBeTruthy();
    const signOut = screen.getByRole("button", { name: "Sign out" }).closest("form");
    expect(signOut?.getAttribute("action")).toBe("/logout");
    expect((signOut?.querySelector('input[name="csrf"]') as HTMLInputElement).value).toBe("csrf-token");
  });

  it.each(["admin", "support", "billing", "viewer", "", undefined])("shows the platform entry point only with a platform role (%s)", async (platform_role) => {
    const navigate = vi.fn();
    renderWith(fakeApi({ organizations: vi.fn(async () => ({ ...listing, platform_role })) }), <OrganizationChooser onNavigate={navigate} />);
    await screen.findByRole("link", { name: "Alpha" });
    const link = screen.queryByRole("link", { name: "Platform console" });
    if (platform_role) {
      expect(link?.getAttribute("href")).toBe("/platform/tenants");
    } else {
      expect(link).toBeNull();
      expect(screen.queryByText("Platform console")).toBeNull();
    }
    expect(navigate).not.toHaveBeenCalled();
  });

  it.each([false, true, undefined])("gates the create row for an existing organization: %s", async (can_create) => {
    renderWith(fakeApi({ organizations: vi.fn(async () => ({ ...listing, can_create })) }), <OrganizationChooser onNavigate={vi.fn()} />);
    await screen.findByRole("link", { name: "Alpha" });
    expect(screen.queryByRole("link", { name: "Create organization" }) !== null).toBe(can_create === true);
  });

  it.each([false, true, undefined])("renders the empty state with creation permission: %s", async (can_create) => {
    const navigate = vi.fn();
    renderWith(fakeApi({ organizations: vi.fn(async () => ({ ...listing, organizations: [], pending: [], can_create })) }), <OrganizationChooser onNavigate={navigate} />);
    await screen.findByRole("heading", { name: "No organizations yet" });
    if (can_create) {
      expect(screen.getByText("Create one to get started, or ask an owner to invite you to theirs.")).toBeTruthy();
      expect(screen.getByText("An organization holds your projects, runners and billing. You become its owner.")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Create organization" }));
      expect(navigate).toHaveBeenCalledWith("/organizations/new");
    } else {
      expect(screen.getByText("Ask an owner to invite you to their organization.")).toBeTruthy();
      expect(screen.queryByText("Create one to get started, or ask an owner to invite you to theirs.")).toBeNull();
      expect(screen.getByText("Ask an owner to send you an invitation.")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Create organization" })).toBeNull();
    }
  });

  it("keeps all pending states in the list and tabs through destinations before sign out", async () => {
    const user = userEvent.setup();
    const pending = [
      { id: "org_wait", name: "Waiting organization", url: "", state: "requested" },
      { id: "org_setup", name: "New organization", url: "", state: "allocating" },
      ...listing.pending,
    ];
    renderWith(fakeApi({ organizations: vi.fn(async () => ({ ...listing, pending, platform_role: "admin" })) }), <OrganizationChooser onNavigate={vi.fn()} />);
    await screen.findByRole("link", { name: "Alpha" });
    for (const label of ["Waiting to start", "Setting up", "Needs attention"]) expect(screen.getByText(label)).toBeTruthy();
    for (const organization of pending) expect(screen.getByRole("link", { name: organization.name }).getAttribute("href")).toBe(`/organizations/${organization.id}/provisioning`);
    for (const name of ["Alpha", "Waiting organization", "New organization", "Beta", "Create organization", "Platform console", "Sign out"]) {
      await user.tab();
      expect(document.activeElement).toBe(screen.getByRole(name === "Sign out" ? "button" : "link", { name }));
    }
  });

  it("keeps the loading text and reports listing failures", async () => {
    const api = fakeApi({ organizations: vi.fn(async () => { throw new AccountError({ status: 503, code: "unavailable", message: "Organizations unavailable" }); }) });
    renderWith(api, <OrganizationChooser onNavigate={vi.fn()} />);
    expect(screen.getByText("Loading your organizations…")).toBeTruthy();
    expect((await screen.findByRole("alert")).textContent).toBe("Organizations unavailable");
  });

  it("sends an expired session back to sign-in", async () => {
    renderWith(
      fakeApi({
        organizations: vi.fn(async () => {
          throw new AccountError({ status: 401, code: "unauthenticated", message: "Sign in" });
        }),
      }),
      <OrganizationChooser onNavigate={vi.fn()} />,
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_ORGANIZATIONS));
  });
});

describe("organization chooser auto-enter", () => {
  const alpha = { id: "org_a", name: "Alpha", url: "/organizations/org_a/organization", role: "owner" };
  const gamma = { id: "org_c", name: "Gamma", url: "/organizations/org_c/organization", role: "member" };

  it.each([
    { name: "a single ready organization opens it", organizations: [alpha], pending: [], search: "", enters: true },
    { name: "a single organization with a pending one stays", organizations: [alpha], pending: listing.pending, search: "", enters: false },
    { name: "an explicit switch stays", organizations: [alpha], pending: [], search: "?switch=1", enters: false },
    { name: "several organizations stay", organizations: [alpha, gamma], pending: [], search: "", enters: false },
  ])("$name", async ({ organizations, pending, search, enters }) => {
    vi.stubGlobal("location", { assign, replace, search, pathname: "/organizations" });
    const api = fakeApi({ organizations: vi.fn(async () => ({ ...listing, organizations, pending })) });
    renderWith(api, <OrganizationChooser onNavigate={vi.fn()} />);
    if (enters) {
      await waitFor(() => expect(replace).toHaveBeenCalledWith(alpha.url));
      expect(screen.queryByRole("link", { name: "Alpha" })).toBeNull();
      return;
    }
    const open = await screen.findByRole("link", { name: "Alpha" });
    expect(open.getAttribute("href")).toBe(alpha.url);
    expect(screen.getByText("Owner")).toBeTruthy();
    expect(replace).not.toHaveBeenCalled();
  });

  it("auto-enters a platform member's single organization", async () => {
    const navigate = vi.fn();
    renderWith(
      fakeApi({ organizations: vi.fn(async () => ({ ...listing, organizations: [alpha], pending: [], platform_role: "admin" })) }),
      <OrganizationChooser onNavigate={navigate} />,
    );
    await waitFor(() => expect(replace).toHaveBeenCalledWith(alpha.url));
    expect(navigate).not.toHaveBeenCalled();
  });

  it("returns to the chooser without entering from the other entry screens", async () => {
    const navigate = vi.fn();
    renderWith(fakeApi(), <CreateOrganization onNavigate={navigate} />);
    fireEvent.click(await screen.findByRole("button", { name: "Back to organizations" }));
    expect(navigate).toHaveBeenCalledWith(CHOOSE_ORGANIZATION);
  });
});

describe("create organization", () => {
  it.each([false, true])("submits with a stable key and the selected plan when the Free slot is used: %s", async (used) => {
    const api = fakeApi({ session: vi.fn(async () => ({ email: listing.email, csrf: listing.csrf, can_create: true, free_slot_used: used, creation_prices: [{ id: "price_starter", label: "Starter" }] })) });
    const navigate = vi.fn();
    renderWith(api, <CreateOrganization onNavigate={navigate} />);
    await waitFor(() => expect(api.session).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText("Organization name"), { target: { value: "  Delta  " } });
    if (used) {
      await screen.findByText(/Your Free organization slot is used/);
      expect((screen.getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(true);
      fireEvent.click(screen.getByRole("combobox", { name: "Paid plan" }));
      fireEvent.click(await screen.findByRole("option", { name: "Starter" }));
      await waitFor(() => expect(screen.getByRole("combobox", { name: "Paid plan" }).textContent).toContain("Starter"));
    }
    await waitFor(() => expect((screen.getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Create organization" }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/organizations/org_new/provisioning"));
    const call = vi.mocked(api.createOrganization).mock.calls[0]![0];
    expect(call.name).toBe("Delta");
    expect(call.price).toBe(used ? "price_starter" : "");
    expect(call.csrf).toBe("csrf-token");
    expect(call.key.length).toBeGreaterThanOrEqual(16);
  });

  it("shows the entry's refusal", async () => {
    renderWith(
      fakeApi({
        session: vi.fn().mockResolvedValueOnce({ email: listing.email, csrf: listing.csrf, can_create: true, free_slot_used: false }).mockResolvedValue({ email: listing.email, csrf: listing.csrf, can_create: true, free_slot_used: true, creation_prices: [{ id: "price_starter", label: "Starter" }] }),
        createOrganization: vi.fn(async () => {
          throw new AccountError({ status: 422, code: "paid_plan_required", message: "Your Free organization slot is used. Choose a paid plan." });
        }),
      }),
      <CreateOrganization onNavigate={vi.fn()} />,
    );
    fireEvent.change(screen.getByLabelText("Organization name"), { target: { value: "Delta" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Create organization" }));
    expect((await screen.findByRole("alert")).textContent).toContain("Free organization slot");
    expect(await screen.findByRole("combobox", { name: "Paid plan" })).toBeTruthy();
  });
});

describe("provisioning progress", () => {
  it("shows a failed setup with its message and resumes it", async () => {
    const provisioning = vi
      .fn()
      .mockResolvedValueOnce({ id: "org_b", name: "Beta", state: "failed", step: "provider_organization", error: "Setup stopped (owner_membership_failed).", can_resume: true })
      .mockResolvedValue({ id: "org_b", name: "Beta", state: "allocating", step: "owner_membership", error: "", can_resume: false });
    const api = fakeApi({ provisioning });
    renderWith(api, <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={10_000} />);
    expect(await screen.findByText("Setup stopped.")).toBeTruthy();
    expect(screen.getByText(/owner_membership_failed/)).toBeTruthy();
    const current = screen.getByText("Making you the owner").closest("li");
    expect(current?.getAttribute("aria-current")).toBe("step");
    await waitFor(() => expect((screen.getByRole("button", { name: "Resume setup" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Resume setup" }));
    await waitFor(() => expect(api.resume).toHaveBeenCalledWith({ organization: "org_b", csrf: "csrf-token" }));
    expect(await screen.findByText("Setting up…")).toBeTruthy();
  });

  it("shows each failed tenant start while retrying, then the stored reason once setup stops", async () => {
    const reason = "the tenant Hub exited 5 times in a row without staying up (last: exit status 1)";
    const provisioning = vi
      .fn()
      .mockResolvedValueOnce({ id: "org_b", name: "Beta", state: "allocating", step: "tenant_files", error: "Attempt 1 of 5 failed: the tenant Hub did not become healthy within 30s. Retrying automatically.", can_resume: false })
      .mockResolvedValue({ id: "org_b", name: "Beta", state: "failed", step: "tenant_files", error: `Setup stopped (tenant_start_failed): ${reason}. Your request and any completed steps are kept.`, can_resume: true });
    renderWith(fakeApi({ provisioning }), <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={5} />);
    expect(await screen.findByText(/Attempt 1 of 5 failed/)).toBeTruthy();
    expect(screen.getByText("Setting up…")).toBeTruthy();
    expect(await screen.findByText("Setup stopped.")).toBeTruthy();
    expect(screen.getByText(new RegExp(reason.replace(/[()]/g, "\\$&")))).toBeTruthy();
    const current = screen.getByText("Starting your Hub").closest("li");
    expect(current?.getAttribute("aria-current")).toBe("step");
    expect(current?.textContent).toContain("!");
    expect(screen.getByRole("button", { name: "Resume setup" })).toBeTruthy();
    const settled = provisioning.mock.calls.length;
    await new Promise((resolve) => globalThis.setTimeout(resolve, 50));
    expect(provisioning.mock.calls.length).toBe(settled);
  });

  it("opens the organization once it is ready", async () => {
    renderWith(
      fakeApi({ provisioning: vi.fn(async () => ({ id: "org_b", name: "Beta", state: "ready", step: "publish", error: "", can_resume: false, next: "/organizations/org_b/" })) }),
      <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} />,
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/organizations/org_b/"));
  });

  it("never overlaps status reads when responses are slower than the poll interval", async () => {
    let inFlight = 0;
    let maxInFlight = 0;
    let calls = 0;
    const provisioning = vi.fn(async () => {
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      calls++;
      await new Promise((resolve) => globalThis.setTimeout(resolve, 30));
      inFlight--;
      return calls >= 3
        ? { id: "org_b", name: "Beta", state: "ready", step: "publish", error: "", can_resume: false, next: "/organizations/org_b/work" }
        : { id: "org_b", name: "Beta", state: "allocating", step: "admission", error: "", can_resume: false };
    });
    renderWith(fakeApi({ provisioning }), <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={5} />);
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/organizations/org_b/work"));
    expect(maxInFlight).toBe(1);
  });

  it("polls while setup is running", async () => {
    const provisioning = vi.fn(async () => ({ id: "org_b", name: "Beta", state: "allocating", step: "admission", error: "", can_resume: false }));
    renderWith(fakeApi({ provisioning }), <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={20} />);
    await waitFor(() => expect(provisioning.mock.calls.length).toBeGreaterThan(2));
  });

  it("orders checkpoints from the last completed step", () => {
    expect(currentStepIndex("")).toBe(0);
    expect(currentStepIndex("admission")).toBe(1);
    expect(currentStepIndex("publish")).toBe(PROVISIONING_STEPS.length - 1);
  });
});

describe("entry API", () => {
  it("reads JSON and posts forms with the CSRF token", async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    const api = makeEntryApi({
      fetch: async (url, init) => {
        calls.push({ url, init });
        return new Response(JSON.stringify(url.startsWith("/api/cloud/platform/users?") ? { users: [], unsearched: [], organizations: [], total: 0, total_users: 0, page: 1, page_size: 50, counts: { all: 0, staff: 0, no_organization: 0, invited: 0 } } : url.startsWith("/api") ? { ...listing, platform_role: "admin" } : { next: "/next" }), { status: 200 });
      },
    });
    expect((await api.organizations()).platform_role).toBe("admin");
    expect((await api.session()).platform_role).toBe("admin");
    await api.createOrganization({ name: "Delta", key: "key_0123456789abcdef", csrf: "csrf-token" });
    expect(calls[0]!.url).toBe("/api/cloud/organizations");
    expect((calls[0]!.init!.headers as Record<string, string>).Accept).toBe("application/json");
    expect(calls[2]!.url).toBe("/organizations");
    expect((calls[2]!.init!.headers as Record<string, string>)["X-CSRF-Token"]).toBe("csrf-token");
    const body = new URLSearchParams(calls[2]!.init!.body as string);
    expect(body.get("name")).toBe("Delta");
    expect(body.get("creation_key")).toBe("key_0123456789abcdef");
    expect(body.get("csrf")).toBe("csrf-token");
    expect(await api.platformUsers("alex+test@example.test")).toEqual({ users: [], unsearched: [], organizations: [], total: 0, total_users: 0, page: 1, page_size: 50, counts: { all: 0, staff: 0, no_organization: 0, invited: 0 } });
    expect(calls[3]!.url).toBe("/api/cloud/platform/users?q=alex%2Btest%40example.test");
    expect(calls[3]!.init?.method).toBe("GET");
    await api.platformUsers("a", { segment: "staff", organization: "org_example", role: "support", sort: "email", direction: "desc", page: 2 });
    expect(calls[4]!.url).toBe("/api/cloud/platform/users?q=a&segment=staff&organization=org_example&role=support&sort=email&direction=desc&page=2");
  });

  it.each([
    { action: "add" as const, method: "POST", suffix: "" },
    { action: "change" as const, method: "PATCH", suffix: "/member%2Bstaff%40example.test" },
    { action: "remove" as const, method: "DELETE", suffix: "/member%2Bstaff%40example.test" },
  ])("sends the $action member command with JSON, CSRF and an encoded target", async ({ action, method, suffix }) => {
    const fetch = vi.fn(async () => new Response(JSON.stringify({ email: "member+staff@example.test", role: "viewer", revision: 5 })));
    const api = makeEntryApi({ fetch });
    await api.changePlatformMember({ csrf: "staff-csrf", change: {
      action, email: "member+staff@example.test", ...(action === "remove" ? {} : { role: "viewer" }),
      reason: "Staffing", expected_revision: 4, idempotency_key: "member-key",
    } });
    expect(fetch).toHaveBeenCalledWith("/api/cloud/platform/members" + suffix, expect.objectContaining({
      method, credentials: "same-origin", headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": "staff-csrf" },
      body: JSON.stringify({ ...(action === "remove" ? {} : { role: "viewer" }), reason: "Staffing", expected_revision: 4,
        idempotency_key: "member-key", ...(action === "add" ? { email: "member+staff@example.test" } : {}) }),
    }));
  });

  it("decodes the entry's error body", async () => {
    const api = makeEntryApi({
      fetch: async () => new Response(JSON.stringify({ code: "intent_conflict", message: "Different name" }), { status: 409 }),
    });
    await expect(api.createOrganization({ name: "x", key: "k", csrf: "c" })).rejects.toMatchObject({
      status: 409,
      code: "intent_conflict",
      message: "Different name",
    });
  });
});

describe("entry router", () => {
  it.each(ENTRY_ROUTE_PATHS.filter((path) => path !== "/platform").map((path) => path.replace("$organization", "org_b").replace("$email", "person%40example.test")))(
    "resolves %s",
    async (path) => {
      const router = makeEntryRouter(createMemoryHistory({ initialEntries: [path] }));
      await router.load();
      expect(router.state.location.pathname).toBe(path);
    },
  );

  it.each([
    ["/platform", "/platform/tenants"],
    ["/platform/accounts", "/platform/users"],
  ])("redirects %s to %s", async (from, to) => {
    const router = makeEntryRouter(createMemoryHistory({ initialEntries: [from] }));
    await router.load();
    expect(router.state.location.pathname).toBe(to);
  });

  it("keeps the explicit switch flag when returning to the chooser", async () => {
    const router = makeEntryRouter(createMemoryHistory({ initialEntries: ["/organizations/new"] }));
    render(
      <EntryApiContext.Provider value={fakeApi()}>
        <RouterProvider router={router} />
      </EntryApiContext.Provider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Back to organizations" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/organizations"));
    expect(router.state.location.searchStr).toContain("switch=");
  });

  it("sends unknown paths to the chooser", async () => {
    const router = makeEntryRouter(createMemoryHistory({ initialEntries: ["/elsewhere"] }));
    await router.load();
    expect(router.state.location.pathname).toBe("/organizations");
  });

  it("recognizes the entry surface marker", () => {
    const meta = (content: string | null) => ({
      querySelector: () => (content === null ? null : { getAttribute: () => content }),
    });
    expect(isEntrySurface(meta("entry") as never)).toBe(true);
    expect(isEntrySurface(meta(null) as never)).toBe(false);
  });
});

describe("organization client behind the shared entry", () => {
  it("knows its organization from the base path", async () => {
    const { behindSharedEntry, currentSharedOrganization } = await import("../../src/app/entry/shared.ts");
    expect(behindSharedEntry("/organizations/org_a")).toBe(true);
    expect(behindSharedEntry("")).toBe(false);
    expect(currentSharedOrganization("/organizations/org_a")).toBe("org_a");
    expect(currentSharedOrganization("")).toBe("");
  });
});
