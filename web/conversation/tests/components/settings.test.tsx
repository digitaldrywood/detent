// @vitest-environment jsdom
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
  Outlet,
  useLocation,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as Schema from "effect/Schema";

import { keySetupPrompt } from "../../src/app/settings/APIKeysMCPSettings.tsx";
import { UsageMeter, formatUsageValue } from "../../src/components/ui/usage-meter.tsx";
import { approachingPlanLimits, nextFittingPlan } from "../../src/app/settings/planUsage.ts";
import type { BillingUsageReport, PlanReport } from "../../src/contracts/account.ts";
import membersFixture from "../../src/contracts/fixtures/account-members.json";
import { OrganizationMembersProvider } from "../../src/app/account/OrganizationMembers.tsx";
import { accountRoutes } from "../../src/app/routes.account.tsx";
import { SettingsSidebarNav } from "../../src/components/settings/SettingsSidebarNav.tsx";
import { SidebarProvider } from "../../src/components/ui/sidebar.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import {
  DEFAULT_SECTION,
  isSettingsSectionId,
  SETTINGS_SECTION_LABELS,
  settingsNavItems,
  type SettingsNavItem,
} from "../../src/app/settings/sections.tsx";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
} from "../../src/app/settings/settingsLayout.tsx";
import { ITEM_ROW_CLASSNAME } from "../../src/app/settings/itemRows.ts";
import { ExpandableText } from "../../src/app/settings/ExpandableText.tsx";
import { ProviderSummary, summarizeProviders } from "../../src/app/fleet/RunnersSection.tsx";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import type { FleetResponse } from "../../src/contracts/account.ts";
import { AccountBootstrap } from "../../src/contracts/account.ts";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import { GeneralSettings, PlanSettings, SettingsRoute } from "../../src/app/settings/Settings.tsx";
import { organizationMCPEndpoint } from "../../src/app/settings/MCPSettings.tsx";
import { applyHubPaths, resetHubPaths } from "../../src/runtime/basePath.ts";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  resetHubPaths();
});

if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const FLEET = fleetFixture as unknown as FleetResponse;

function labelsFor(items: readonly SettingsNavItem[]): readonly string[] {
  return items.map((item) => item.label);
}

describe("which sections an actor gets", () => {
  beforeEach(() => applyHubPaths({ base_path: "/organizations/org_test" }));
  it("gives an owner every Detent section, in the reading order", () => {
    const items = settingsNavItems({ canManage: true, supporting: false });
    expect(labelsFor(items)).toEqual([
      "General",
      "Organization",
      "Members",
      "Models",
      "Projects",
      "Providers & runners",
      "Integrations",
      "API keys & MCP",
      "Plan & usage",
      "Billing",
      "Appearance",
      "Keybindings",
      "SnapShots",
      "Source Control",
      "Connections",
      "Archive",
      "What's new",
      "About",
    ]);
  });

  it("hides plan and billing from somebody who could only be refused them", () => {
    const items = settingsNavItems({ canManage: false, supporting: false });
    expect(labelsFor(items)).not.toContain("Plan & usage");
    expect(labelsFor(items)).not.toContain("Billing");
    expect(items.slice(-2).map((item) => item.id)).toEqual(["whats-new", "about"]);
  });

  it("closes billing during a support session, and leaves the plan open", () => {
    const items = settingsNavItems({ canManage: true, supporting: true });
    expect(labelsFor(items)).toContain("Plan & usage");
    expect(labelsFor(items)).not.toContain("Billing");
  });

  it("keeps the unavailable sections present and disabled rather than dropping them", () => {
    const items = settingsNavItems({ canManage: true, supporting: false });
    const disabled = items.filter((item) => item.disabled === true);
    expect(labelsFor(disabled)).toEqual([
      "Appearance",
      "SnapShots",
      "Source Control",
      "Connections",
      "Archive",
    ]);
    for (const item of disabled) expect(item.reason).toBeTruthy();
  });

  it.each(["admin", "support", "billing", "viewer", "", undefined])("shows the platform account link only with a platform role (%s)", async (platform_role) => {
    applyHubPaths({ base_path: "/organizations/org_test" });
    renderSidebarNav("/settings/general", {
      http: { origin: "", apiBase: "/api", csrfToken: "c" },
      account: {
        ...OWNER_ACCOUNT.account,
        actor: { ...OWNER_ACCOUNT.account.actor, email: "member@example.test", role: "member", platform_role, can_manage: false },
        support: null, plan: null,
      },
    }, <GeneralSettings />);
    await screen.findByRole("button", { name: "Sign out" });
    const link = screen.queryByRole("link", { name: "Platform console" });
    if (platform_role) {
      expect(link?.getAttribute("href")).toBe("/platform/tenants");
    } else {
      expect(link).toBeNull();
    }
  });

  it.each(["", "/organizations/org_test"])("limits personal-key navigation to shared Cloud (%s)", async (mount) => {
    applyHubPaths({ base_path: mount });
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => new Response(JSON.stringify(
      String(url).endsWith("key-context") ? { organizations: [], mcp_endpoint: "https://cloud.example.test/mcp" } : { keys: [] },
    )));
    renderSidebarNav("/settings/api-keys", { ...OWNER_ACCOUNT, bootstrap: { organization: OWNER_ACCOUNT.account.organization } }, <SettingsRoute section="api-keys" />);
    await screen.findByRole("button", { name: "General" });
    if (mount) {
      await screen.findByRole("button", { name: "Create key" });
      expect(screen.getByRole("button", { name: "API keys & MCP" })).toBeTruthy();
    } else {
      expect(screen.queryByRole("button", { name: "API keys" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Create key" })).toBeNull();
      expect(fetch.mock.calls.some(([url]) => String(url).startsWith("/api/cloud/"))).toBe(false);
    }
  });

  it("names every section it can route to", () => {
    for (const id of Object.keys(SETTINGS_SECTION_LABELS)) {
      expect(isSettingsSectionId(id)).toBe(true);
    }
    expect(isSettingsSectionId("appearance")).toBe(false);
    expect(isSettingsSectionId(DEFAULT_SECTION)).toBe(true);
  });
});

function renderSidebarNav(pathname = "/settings/projects", account: unknown = OWNER_ACCOUNT, content: React.ReactNode = null, served = false) {
  const navigated: string[] = [];
  const root = createRootRoute({ component: served ? () => {
    const current = useLocation({ select: (location) => location.pathname });
    return <OrganizationMembersProvider><SidebarProvider><SettingsSidebarNav pathname={current} /><Outlet /></SidebarProvider></OrganizationMembersProvider>;
  } : undefined });
  const section = createRoute({
    getParentRoute: () => root,
    path: "/settings/$section",
    component: () => (
      <OrganizationMembersProvider><SidebarProvider>
        <SettingsSidebarNav pathname={pathname} />
        {content}
      </SidebarProvider></OrganizationMembersProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren(served ? accountRoutes(root) : [section, createRoute({ getParentRoute: () => root, path: "/settings/organization/$organizationPage", component: section.options.component })]),
    history: createMemoryHistory({ initialEntries: [pathname] }),
  });
  const subscribe = router.subscribe("onResolved", (event) => {
    navigated.push(event.toLocation.pathname);
  });
  render(
    <ClientContext.Provider value={account as ConversationClient}>
      <RouterProvider router={router as never} />
    </ClientContext.Provider>,
  );
  return { navigated, subscribe, router };
}

const OWNER_ACCOUNT = {
  http: { origin: "", apiBase: accountFixture.api_base, csrfToken: accountFixture.csrf_token },
  account: Schema.decodeUnknownSync(AccountBootstrap)(accountFixture),
};

describe("issue notification preference", () => {
  it.each([false, true])("loads the saved preference (%s) and persists a toggle", async (enabled) => {
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      if (String(url).endsWith("issue-notification-preference")) {
        return new Response(JSON.stringify({ enabled: init?.method === "PUT" ? !enabled : enabled }));
      }
      return new Response(JSON.stringify({ projects: [] }));
    });
    renderSidebarNav("/settings/general", OWNER_ACCOUNT, <GeneralSettings />);
    const toggle = await screen.findByRole("switch", { name: "Notify me about issues I filed or commented on" });
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe(String(enabled)));
    fireEvent.click(toggle);
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe(String(!enabled)));
    expect(fetch.mock.calls.some(([url, init]) => String(url).endsWith("issue-notification-preference") && init?.method === "PUT" && init.body === JSON.stringify({ enabled: !enabled }) && (init.headers as Record<string, string>)["X-CSRF-Token"] === accountFixture.csrf_token)).toBe(true);
  });

  it("keeps the saved preference and shows a failed save", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      if (String(url).endsWith("issue-notification-preference")) {
        return init?.method === "PUT"
          ? new Response(JSON.stringify({ code: "unavailable", message: "Notification preference could not be saved" }), { status: 503 })
          : new Response(JSON.stringify({ enabled: true }));
      }
      return new Response(JSON.stringify({ projects: [] }));
    });
    renderSidebarNav("/settings/general", OWNER_ACCOUNT, <GeneralSettings />);
    const toggle = await screen.findByRole("switch", { name: "Notify me about issues I filed or commented on" });
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("true"));
    fireEvent.click(toggle);
    await screen.findByText("Notification preference could not be saved");
    expect(toggle.getAttribute("aria-checked")).toBe("true");
  });
});

describe("MCP setup", () => {
  const account = accountFixture as unknown as AccountBootstrap;

  it.each([
    ["dedicated", "", "https://organization.example.com/", "https://organization.example.com/mcp"],
    ["shared origin", "/organizations/org_example", "https://app.example.org", "https://app.example.org/organizations/org_example/mcp"],
    ["shared entry", "/organizations/org_example", "https://app.example.org/organizations/org_example", "https://app.example.org/organizations/org_example/mcp"],
    ["shared trailing slash", "/organizations/org_example/", "https://app.example.org/organizations/org_example/", "https://app.example.org/organizations/org_example/mcp"],
    ["mounted public URL without runtime mount", "", "https://app.example.org/organizations/org_example", "https://app.example.org/organizations/org_example/mcp"],
    ["another organization's URL", "/organizations/org_example", "https://app.example.org/organizations/org_secondary_example", null],
    ["another organization's URL without runtime mount", "", "https://app.example.org/organizations/org_secondary_example", null],
    ["another organization's mount", "/organizations/org_secondary_example", "https://app.example.org", null],
    ["conflicting public path", "/organizations/org_example", "https://app.example.org/projects/proj_example_project", null],
    ["HTTP", "", "http://organization.example.com", null],
    ["credentials in URL", "/organizations/org_example", "https://user:secret@organization.example.com", null],
    ["query credential", "/organizations/org_example", "https://organization.example.com?token=secret", null],
    ["fragment", "/organizations/org_example", "https://organization.example.com#secret", null],
    ["malformed", "", "not a URL", null],
  ])("uses only a safe canonical %s organization endpoint", (_name, mount, publicURL, expected) => {
    applyHubPaths({ base_path: mount });
    const decoded = Schema.decodeUnknownSync(AccountBootstrap)({ ...account, organization: { ...account.organization, public_url: publicURL }, organizations: [] });
    expect(organizationMCPEndpoint(decoded)).toBe(expected);
  });

  it("matches the active organization's identity rather than another directory entry", () => {
    expect(organizationMCPEndpoint({ ...account, organizations: [...account.organizations].reverse() })).toBe("https://organization.example.com/mcp");
    expect(organizationMCPEndpoint({ ...account, organization: { id: "org_unknown", name: "Unknown" } })).toBeNull();
    expect(organizationMCPEndpoint(null)).toBeNull();
  });

  it.each([
    ["", "https://organization.example.com", "https://organization.example.com/mcp"],
    ["/organizations/org_example", "https://app.example.org", "https://app.example.org/organizations/org_example/mcp"],
    ["/organizations/org_example", "https://app.example.org/organizations/org_example", "https://app.example.org/organizations/org_example/mcp"],
    ["/organizations/org_example", "https://app.example.org/organizations/org_secondary_example", null],
  ])("routes a viewer to setup at %j and copies only a matching organization URL using shared key metadata", async (mount, publicURL, expected) => {
    applyHubPaths({ base_path: mount });
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({ keys: [] }), { status: 200 }));
    const activeAccount = {
      ...account,
      organization: { ...account.organization, public_url: publicURL },
      actor: { ...account.actor, can_manage: false, role: "viewer" },
      csrf_token: "private-csrf-credential",
      token: "private-api-credential",
    };
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: "/api/v2/organizations/org_example", csrfToken: activeAccount.csrf_token }, account: activeAccount, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    if (mount) {
      await screen.findByRole("tab", { name: /^Personal / });
      expect(screen.queryByRole("heading", { name: "Member keys" })).toBeNull();
      expect(screen.queryByRole("heading", { name: "Personal key policy" })).toBeNull();
      expect(fetch.mock.calls.some(([url]) => /external-keys|key-policy|service-keys/.test(String(url)))).toBe(false);
      return;
    }
    await waitFor(() => expect(screen.getByRole("heading", { name: "API keys" })).toBeTruthy());
    expect(screen.getByRole("button", { name: "API & MCP" }).getAttribute("aria-current")).toBe("true");
    expect(document.body.textContent).not.toContain("private-csrf-credential");
    expect(document.body.textContent).not.toContain("private-api-credential");
    if (expected) {
      fireEvent.click(screen.getByRole("button", { name: "Copy MCP endpoint" }));
      await waitFor(() => expect(writeText).toHaveBeenLastCalledWith(expected));
      expect(screen.getByRole("status", { name: "MCP endpoint copy status" }).textContent).toContain("copied");
      await waitFor(() => expect(screen.getByRole("button", { name: "Copy Direct API setup prompt" })).toBeTruthy());
      for (const label of ["Direct API setup prompt", "MCP setup prompt"]) {
        fireEvent.click(screen.getByRole("button", { name: `Copy ${label}` }));
        await waitFor(() => expect(writeText.mock.lastCall![0]).toContain("DETENT_API_KEY"));
        const copied = writeText.mock.lastCall![0];
        expect(copied).toContain(expected.replace(/\/mcp$/, "/settings/mcp"));
        expect(copied).not.toContain("private-csrf-credential");
        expect(copied).not.toContain("private-api-credential");
        expect(copied).toContain(label.startsWith("Direct") ? "/work-items?limit=20" : "work_list");
      }
    } else {
      expect(screen.queryByRole("button", { name: "Copy MCP endpoint" })).toBeNull();
      expect(screen.getByText(/The organization HTTPS URL is unavailable/)).toBeTruthy();
      expect(writeText).not.toHaveBeenCalled();
    }
    expect(screen.queryByRole("button", { name: "Copy Local stdio configuration" })).toBeNull();
    for (const [url, options] of fetch.mock.calls) {
      expect(String(url)).not.toMatch(/tokens|credentials/);
      expect(options?.method ?? "GET").toBe("GET");
    }
  });

  it.each(["all", "selected", "all before projects"] as const)("creates and revokes a %s key while keeping it out of copied prompts", async (mode) => {
    const currentAccount = mode === "all before projects" ? { ...account, projects: [] } : account;
    const projectAccess = mode === "selected" ? "selected" : "all";
    const projectIds = mode === "selected" ? [account.projects[0]!.id] : [];
    const secret = "detent_synthetic_once_only_key";
    const metadata = { id: "key1", name: "agent", scope: "read", expires_at: "2099-11-01T00:00:00Z", created_at: "2026-10-01T12:00:00Z", fingerprint: "fingerprint", revoked: false, project_access: projectAccess, project_ids: projectIds };
    let created = false;
    let revoked = false;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, options) => {
      if (options?.method === "POST") { created = true; return new Response(JSON.stringify({ token: secret, project_access: projectAccess, project_ids: projectIds }), { status: 201 }); }
      if (options?.method === "DELETE") { revoked = true; return new Response(null, { status: 204 }); }
      return new Response(JSON.stringify({ keys: created ? [{ ...metadata, revoked, ...(revoked ? { revoked_at: "2026-10-02T12:00:00Z" } : {}) }] : [] }));
    });
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: account.api_base, csrfToken: "browser-csrf" }, account: currentAccount, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Create key" })).toBeTruthy());
    expect(screen.queryByLabelText("Name")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create key" }));
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "agent" } });
    expect(screen.getByRole("radio", { name: "All projects" })).toHaveProperty("checked", true);
    expect(screen.queryAllByRole("checkbox")).toHaveLength(0);
    expect(within(screen.getByRole("dialog")).getByRole("button", { name: "Create key" })).toHaveProperty("disabled", false);
    if (mode === "selected") {
      fireEvent.click(screen.getByRole("radio", { name: "Selected projects" }));
      expect(within(screen.getByRole("dialog")).getByRole("button", { name: "Create key" })).toHaveProperty("disabled", true);
      fireEvent.click(screen.getByRole("checkbox", { name: account.projects[0]!.name }));
    }
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Create key" }));
    expect(screen.getByLabelText("Name")).toHaveProperty("disabled", true);
    expect(screen.getByRole("radio", { name: "Read" })).toHaveProperty("disabled", true);
    expect(screen.getByLabelText("Expires")).toHaveProperty("disabled", true);
    await waitFor(() => expect(screen.getByLabelText("New API key")).toBeTruthy());
    expect(screen.getByLabelText("New API key").getAttribute("type")).toBe("password");
    expect(document.body.textContent).not.toContain(secret);
    fireEvent.click(screen.getByRole("button", { name: "Copy key privately" }));
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith(secret));
    const post = fetch.mock.calls.find((call) => call[1]?.method === "POST")!;
    expect(post[0]).toBe(`${account.api_base}/api-keys`);
    expect(post[1]?.headers).toMatchObject({ "X-CSRF-Token": "browser-csrf" });
    expect(JSON.parse(post[1]!.body as string)).toMatchObject({ name: "agent", scope: "read", expires_days: 30, project_access: projectAccess, project_ids: projectIds });
    for (const label of ["Direct API prompt", "MCP prompt"]) {
      const copies = writeText.mock.calls.length;
      fireEvent.click(screen.getByRole("button", { name: `Copy ${label}` }));
      await waitFor(() => expect(writeText).toHaveBeenCalledTimes(copies + 1));
      expect(writeText.mock.lastCall![0]).toContain(mode === "selected" ? `Project access: Selected projects: ${account.projects[0]!.name}` : "Project access: All projects, including future projects");
      expect(writeText.mock.lastCall![0]).not.toContain(secret);
    }
    if (mode === "selected") {
      fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    } else if (mode === "all before projects") {
      fireEvent.click(screen.getByRole("button", { name: "Close" }));
    } else {
      fireEvent.click(screen.getByRole("button", { name: "Done" }));
    }
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByLabelText("New API key")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create key" }));
    await screen.findByRole("heading", { name: "Create API key" });
    expect(screen.queryByLabelText("New API key")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Revoke agent" }));
    await screen.findByRole("heading", { name: "Revoke agent?" });
    expect(fetch.mock.calls.some((call) => call[1]?.method === "DELETE")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Keep key" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Revoke agent" }));
    fireEvent.click(await screen.findByRole("button", { name: "Revoke key" }));
    await screen.findByRole("button", { name: "Key history · 1 revoked or expired" });
    expect(screen.queryByRole("heading", { name: /^agent/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Key history · 1 revoked or expired" }));
    expect(await screen.findByText("Revoked")).toBeTruthy();
    expect(fetch.mock.calls.some((call) => call[0] === `${account.api_base}/api-keys/key1` && call[1]?.method === "DELETE")).toBe(true);
  });

  it.each([
    ["viewer", ["Read"]],
    ["member", ["Read", "Write"]],
    ["admin", ["Read", "Write", "Admin"]],
    ["owner", ["Read", "Write", "Admin"]],
  ])("offers only %s permissions with explanations", async (role, labels) => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({ keys: [] })));
    const currentAccount = { ...account, actor: { ...account.actor, role, can_manage: role === "owner" || role === "admin" } };
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: account.api_base, csrfToken: account.csrf_token }, account: currentAccount, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    fireEvent.click(await screen.findByRole("button", { name: "Create key" }));
    const group = await screen.findByRole("group", { name: "Permissions" });
    expect(within(group).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label"))).toEqual(labels);
    for (const radio of within(group).getAllByRole("radio")) {
      expect(document.getElementById(radio.getAttribute("aria-describedby")!)?.textContent).toBeTruthy();
    }
    expect(screen.getByLabelText("Expires")).toHaveProperty("value", "30");
  });

  it("separates inactive keys, localizes dates and resolves project names with an ID fallback", async () => {
    const key = { scope: "read", fingerprint: "12345678901234567890", created_at: "2026-10-01T12:00:00.123456789Z", expires_at: "2099-11-01T12:00:00Z", revoked: false, project_access: "selected", project_ids: [account.projects[0]!.id, "unknown-project"] };
    vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({ keys: [
      { ...key, id: "active", name: "Active agent" },
      { ...key, id: "never", name: "Long-lived agent", expires_at: null },
      { ...key, id: "revoked", name: "Revoked agent", revoked: true, revoked_at: "2026-10-02T12:00:00Z" },
      { ...key, id: "expired", name: "Expired agent", expires_at: "2020-01-01T12:00:00Z" },
    ] })));
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: account.api_base, csrfToken: account.csrf_token }, account, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    const permanent = (await screen.findByRole("heading", { name: /Long-lived agent/ })).closest('[data-slot="settings-row"]')!;
    expect(permanent.textContent).toContain("Never expires");
    const active = (await screen.findByRole("heading", { name: /Active agent/ })).closest('[data-slot="settings-row"]')!;
    expect(active.textContent).toContain(account.projects[0]!.name);
    expect(active.textContent).toContain("unknown-project");
    expect(active.textContent).toContain("created Oct 1");
    expect(active.textContent).toContain("123456789012");
    expect(active.textContent).not.toContain("12345678901234567890");
    expect(active.textContent).not.toContain("T12:00:00");
    expect(screen.queryByText("Revoked")).toBeNull();
    expect(screen.queryByText("Expired")).toBeNull();
    const toggle = screen.getByRole("button", { name: "Key history · 2 revoked or expired" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(screen.getByText("Revoked")).toBeTruthy();
    expect(screen.getByText("Expired")).toBeTruthy();
    expect(screen.getByText(/Revoked Oct 2, 2026/)).toBeTruthy();
    expect(screen.getByText(/Expired Jan 1, 2020/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Revoke Revoked agent" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Revoke Expired agent" })).toBeNull();
    fireEvent.click(toggle);
    expect(screen.queryByText("Revoked")).toBeNull();
  });

  it("keeps setup unavailable when shared key authentication is not installed", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 404 }));
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: account.api_base, csrfToken: account.csrf_token }, account, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("unavailable"));
    expect(screen.queryByRole("button", { name: "Copy MCP setup prompt" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Create key" })).toBeNull();
  });

  it("offers manual copying when clipboard access fails", async () => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: vi.fn().mockRejectedValue(new Error("denied")) } });
    vi.spyOn(console, "error").mockImplementation(() => {});
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: "/api/v2/organizations/org_example", csrfToken: account.csrf_token }, account, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Copy MCP endpoint" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Copy MCP endpoint" }));
    await waitFor(() => expect(screen.getByRole("status", { name: "MCP endpoint copy status" }).textContent).toContain("Select the text"));
  });
});

describe("personal keys and org controls", () => {
  beforeEach(() => applyHubPaths({ base_path: "/organizations/org_test" }));
  const account = OWNER_ACCOUNT.account;
  const client = { ...OWNER_ACCOUNT, bootstrap: { organization: account.organization } };
  const reach = { organization_id: account.organization.id, name: account.organization.name, role: "member", status: "allowed", projects: [{ id: account.projects[0]!.id, name: account.projects[0]!.name, can_write: true }], last_used_at: null };
  const key = { id: "personal1", name: "My agent", kind: "personal", owner: "user1", owner_email: "member@example.test", permission: "write", access_context: "global", organizations: [], created_at: "2026-10-01T12:00:00Z", effective_reach: [reach], last_used_at: null };
  const context = { organizations: [reach, { ...reach, organization_id: "org_other", name: "Other org", projects: [{ id: "prj_other", name: "Other project", can_write: false }] }], mcp_endpoint: "https://cloud.example.test/mcp" };

  it.each([
    ["member", false, "global", "personal", "global"],
    ["owner", true, "all", "personal", "selected"],
    ["admin", true, "project", "personal", "project"],
    ["owner", true, "all", "service", "selected"],
    ["admin", true, "selected", "service", "selected"],
    ["owner", true, "project", "service", "project"],
  ])("creates a %s key with %s management and %s reach as %s", async (role, canManage, reachChoice, kind, accessContext) => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      if (String(url).endsWith("key-context")) return new Response(JSON.stringify(context));
      if (String(url).endsWith("key-policy")) return new Response(JSON.stringify({ personal_keys: "allowed" }));
      if (init?.method === "POST") return new Response(JSON.stringify({ ...key, kind, ...JSON.parse(String(init.body)), token: "private-new-key" }), { status: 201 });
      return new Response(JSON.stringify({ keys: String(url).includes("/account/api-keys") ? [key] : [] }));
    });
    renderSidebarNav("/settings/mcp", { ...client, account: { ...account, actor: { ...account.actor, can_manage: canManage, role } } } as ConversationClient, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Create key" })).toHaveProperty("disabled", false));
    for (const title of ["API keys & MCP", "Connect an agent"]) expect(screen.getByRole("heading", { name: title })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Personal 1" })).toBeTruthy();
    expect(Boolean(screen.queryByRole("tab", { name: "Service 0" }))).toBe(canManage);
    expect(Boolean(screen.queryByRole("heading", { name: "Member keys" }))).toBe(canManage);
    expect(screen.getAllByRole("button", { name: "Create key" })).toHaveLength(1);
    expect(screen.getAllByRole("textbox", { name: "MCP endpoint value" })).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: "Which key do I need?" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Connect" })).toBeNull();
    expect(screen.queryByText("API and MCP access")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Connect My agent" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Copy MCP endpoint" }));
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith("https://cloud.example.test/mcp"));
    for (const [transport, label] of [["mcp", "MCP"], ["rest", "REST"]]) {
      fireEvent.click(screen.getByRole("button", { name: transport === "mcp" ? "MCP" : "REST API" }));
      const copies = writeText.mock.calls.length;
      fireEvent.click(screen.getByRole("button", { name: `Copy ${label} setup prompt` }));
      await waitFor(() => expect(writeText).toHaveBeenCalledTimes(copies + 1));
      const copiedPrompt = writeText.mock.lastCall![0];
      expect(copiedPrompt).toContain("Store it privately as DETENT_API_KEY");
      if (transport === "mcp") {
        expect(copiedPrompt).toContain('Configure one Streamable HTTP MCP server at https://cloud.example.test/mcp, with bearer_token_env_var = "DETENT_API_KEY"');
        expect(copiedPrompt).toContain('Call connection_info and confirm key.kind is "personal" or "service" and explain who it acts as before reporting success');
        expect(copiedPrompt).not.toContain("Use Authorization: Bearer");
      } else {
        expect(copiedPrompt).toContain("Use Authorization: Bearer with that private environment variable at https://cloud.example.test/api/v2.");
        expect(copiedPrompt).toContain("Read GET /api/v2/organizations");
        expect(copiedPrompt).not.toContain("connection_info");
        expect(copiedPrompt).not.toContain("https://cloud.example.test/mcp");
      }
      const previewButton = screen.getByRole("button", { name: `Preview ${label} setup prompt` });
      fireEvent.click(previewButton);
      expect(screen.getByLabelText(`${label} setup prompt preview`).textContent).toBe(writeText.mock.lastCall![0]);
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(previewButton.getAttribute("aria-expanded")).toBe("true");
      fireEvent.click(previewButton);
      expect(screen.queryByLabelText(`${label} setup prompt preview`)).toBeNull();
    }
    fireEvent.click(screen.getByRole("button", { name: "MCP" }));
    fireEvent.click(screen.getByRole("button", { name: "Create key" }));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByRole("radio", { name: "You" }).getAttribute("aria-checked")).toBe("true");
    if (!canManage) {
      expect(dialog.getByRole("radio", { name: "The organization" }).getAttribute("aria-disabled")).toBe("true");
      expect(dialog.getByText("Owners and admins create service keys.")).toBeTruthy();
    }
    if (kind === "service") fireEvent.click(dialog.getByRole("radio", { name: "The organization" }));
    fireEvent.change(dialog.getByLabelText("Name"), { target: { value: "Agent key" } });
    fireEvent.change(dialog.getByLabelText("Reach"), { target: { value: reachChoice } });
    if (["project", "selected"].includes(reachChoice)) {
      expect(dialog.getByRole("button", { name: `Create ${kind} key` })).toHaveProperty("disabled", true);
      if (reachChoice === "project") fireEvent.change(dialog.getByLabelText(`Project in ${account.organization.name}`), { target: { value: account.projects[0]!.id } });
      else fireEvent.click(dialog.getByRole("checkbox", { name: account.projects[0]!.name }));
    }
    fireEvent.click(dialog.getByRole("button", { name: `Create ${kind} key` }));
    await screen.findByLabelText("New API key");
    const post = fetch.mock.calls.find(([, init]) => init?.method === "POST")!;
    expect(post[0]).toBe(kind === "personal" ? "/api/cloud/account/api-keys" : `${account.api_base.replace("/api/v2", "/api/cloud")}/service-keys`);
    expect(JSON.parse(String(post[1]?.body))).toMatchObject({ access_context: accessContext, organizations: reachChoice === "global" ? [] : [{ organization_id: account.organization.id, project_access: ["project", "selected"].includes(reachChoice) ? reachChoice : "all", project_ids: ["project", "selected"].includes(reachChoice) ? [account.projects[0]!.id] : [] }] });
    expect(dialog.getByRole("heading", { name: "Agent key is ready" })).toBeTruthy();
    expect(dialog.getByText(kind === "personal" ? "Personal · acts as you" : "Service · acts as the organization")).toBeTruthy();
    expect(dialog.getByRole("heading", { name: kind === "personal" ? "Check it connected as you" : "Check it connected" })).toBeTruthy();
    expect(dialog.getByText("Waiting for the first call…")).toBeTruthy();
    expect(dialog.getByText("Copy the key before closing. It cannot be shown again.")).toBeTruthy();
    expect(screen.getByLabelText("New API key").getAttribute("type")).toBe("password");
    expect(document.body.textContent).not.toContain("private-new-key");
    fireEvent.click(dialog.getByRole("button", { name: "Reveal" }));
    expect(screen.getByLabelText("New API key").getAttribute("type")).toBe("text");
    fireEvent.click(dialog.getByRole("button", { name: "Hide" }));
    expect(screen.getByLabelText("New API key").getAttribute("type")).toBe("password");
    for (const [label, expected] of [["Copy key", "private-new-key"], ["Copy as export DETENT_API_KEY=… line", "export DETENT_API_KEY='private-new-key'"]]) {
      const button = dialog.getByRole("button", { name: label });
      fireEvent.click(button);
      await waitFor(() => {
        expect(writeText).toHaveBeenLastCalledWith(expected);
        expect(button.textContent).toBe("Copied");
      });
      expect(dialog.getByLabelText("Step 1 complete")).toBeTruthy();
      expect(dialog.getByText("You can close this. The key stays in your list.")).toBeTruthy();
    }
    for (const [transport, label] of [["mcp", "MCP"], ["rest", "REST API"]]) {
      fireEvent.click(dialog.getByRole("button", { name: label }));
      const preview = dialog.getByLabelText(`${label} setup prompt preview`);
      expect(preview.textContent).toContain(transport === "mcp" ? "connection_info" : "Authorization: Bearer");
      const button = dialog.getByRole("button", { name: `Copy ${label} setup prompt` });
      const copies = writeText.mock.calls.length;
      fireEvent.click(button);
      await waitFor(() => {
        expect(writeText).toHaveBeenCalledTimes(copies + 1);
        expect(button.textContent).toBe("Copied");
      });
      const copiedPrompt = writeText.mock.lastCall![0];
      expect(copiedPrompt).toContain(transport === "mcp" ? `Call connection_info and confirm key.kind is "${kind}", matching the key just created before reporting success` : "Use Authorization: Bearer with that private environment variable at https://cloud.example.test/api/v2");
      expect(copiedPrompt).toContain("DETENT_API_KEY");
      expect(copiedPrompt).not.toContain("private-new-key");
    }
    fireEvent.click(dialog.getByRole("button", { name: "Show full prompt" }));
    expect(dialog.getByLabelText("REST API setup prompt preview").textContent).toContain("Current membership");
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByLabelText("New API key")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Copy MCP setup prompt" }));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    expect(writeText.mock.lastCall![0]).toContain("Settings → API keys & MCP");
    expect(writeText.mock.lastCall![0]).not.toContain("private-new-key");
  });

  it.each([
    ["personal", true, "Example Member"],
    ["personal", false, "member@example.test"],
    ["service", true, `${account.organization.name} (service)`],
    ["service", false, `${account.organization.name} (service)`],
  ])("connects an existing %s key with used=%s without revealing or requesting a token", async (kind, used, actor) => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      const path = String(url);
      if (path.endsWith("key-context")) return new Response(JSON.stringify(context));
      if (path.endsWith("key-policy")) return new Response(JSON.stringify({ personal_keys: "allowed" }));
      if (path.endsWith("members")) return new Response(JSON.stringify({ members: [{ id: "membership1", user_id: key.owner, email: key.owner_email, name: used ? "Example Member" : "", role: "member", status: "active", grants: [] }], invitations: [] }));
      const ownList = kind === "personal" ? path.endsWith("/account/api-keys") : path.endsWith("/service-keys");
      return new Response(JSON.stringify({ keys: ownList ? [
        { ...key, kind, last_used_at: used ? new Date(Date.now() - 120_000).toISOString() : null, token: "must-never-render" },
        { ...key, id: "second", kind, name: "Other agent" },
        { ...key, id: "expired", kind, name: "Expired agent", expires_at: "2020-01-01T00:00:00Z" },
        { ...key, id: "revoked", kind, name: "Revoked agent", revoked_at: "2020-01-01T00:00:00Z" },
      ] : [] }));
    });
    renderSidebarNav("/settings/mcp", client, <SettingsRoute section="mcp" />);
    if (kind === "service") fireEvent.click(await screen.findByRole("tab", { name: "Service 2" }));
    fireEvent.click(await screen.findByRole("button", { name: "Connect My agent" }));
    const dialogElement = screen.getByRole("dialog");
    const dialog = within(dialogElement);
    expect(dialog.getByRole("heading", { name: "Connect My agent" })).toBeTruthy();
    expect(dialog.getByText(kind === "personal" ? "Personal · acts as you" : "Service · acts as the organization")).toBeTruthy();
    expect(dialog.getByRole("heading", { name: "Use the key you saved" })).toBeTruthy();
    expect(dialog.getByText(/The token was shown once/).textContent).toContain("DETENT_API_KEY");
    expect(dialog.queryByLabelText("New API key")).toBeNull();
    expect(dialog.queryByRole("button", { name: "Reveal" })).toBeNull();
    expect(dialog.queryByRole("button", { name: "Copy key" })).toBeNull();
    expect(dialogElement.outerHTML).not.toContain("must-never-render");
    if (used) {
      await waitFor(() => expect(dialog.getByText(/Last call/).textContent).toContain(`2m ago · acting as ${actor}`));
    } else {
      expect(dialog.getByText("Waiting for the first call…")).toBeTruthy();
    }
    for (const [transport, label] of [["mcp", "MCP"], ["rest", "REST"]]) {
      fireEvent.click(dialog.getByRole("button", { name: transport === "mcp" ? "MCP" : "REST API" }));
      const copies = writeText.mock.calls.length;
      fireEvent.click(dialog.getByRole("button", { name: `Copy ${label} setup prompt` }));
      await waitFor(() => expect(writeText).toHaveBeenCalledTimes(copies + 1));
      const copiedPrompt = writeText.mock.lastCall![0];
      expect(copiedPrompt).toContain("Store it privately as DETENT_API_KEY");
      if (transport === "mcp") {
        expect(copiedPrompt).toContain('Configure one Streamable HTTP MCP server at https://cloud.example.test/mcp, with bearer_token_env_var = "DETENT_API_KEY"');
        expect(copiedPrompt).toContain(kind === "personal"
          ? 'Call connection_info and confirm key.kind is "personal", matching the key just created before reporting success'
          : 'Call connection_info and confirm key.kind is "service", matching the key just created before reporting success');
        expect(copiedPrompt).not.toContain("Use Authorization: Bearer");
      } else {
        expect(copiedPrompt).toContain("Use Authorization: Bearer with that private environment variable at https://cloud.example.test/api/v2.");
        expect(copiedPrompt).toContain("Read GET /api/v2/organizations");
        expect(copiedPrompt).not.toContain("connection_info");
        expect(copiedPrompt).not.toContain("https://cloud.example.test/mcp");
      }
      expect(dialog.getByLabelText(`${label} setup prompt preview`).textContent).toBe(writeText.mock.lastCall![0]);
      expect(writeText.mock.lastCall![0]).not.toContain("must-never-render");
    }
    if (kind === "service") {
      expect(dialog.getByText("The token was shown once, when the key was created. Your agent reads it from DETENT_API_KEY. Lost it? Create a replacement key.")).toBeTruthy();
      expect(dialog.queryByRole("button", { name: "Rotate this key" })).toBeNull();
    }
    fireEvent.click(dialog.getByRole("button", { name: "Done" }));
    fireEvent.click(screen.getByRole("button", { name: "Connect Other agent" }));
    expect(screen.getByRole("heading", { name: "Connect Other agent" })).toBeTruthy();
    expect(within(screen.getByRole("dialog")).getByText("Waiting for the first call…")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Show revoked and expired" }));
    const table = within(screen.getByRole("table", { name: kind === "personal" ? "Personal keys" : "Service keys" }));
    for (const name of ["Expired agent", "Revoked agent"]) {
      const row = within(table.getByText(name).closest("tr")!);
      expect(row.queryByRole("button", { name: /Connect|More actions/ })).toBeNull();
    }
    expect(table.getByRole("button", { name: "Connect My agent" })).toBeTruthy();
    fireEvent.click(screen.getByRole("checkbox", { name: "Show revoked and expired" }));
    expect(table.queryByText("Expired agent")).toBeNull();
    expect(table.queryByText("Revoked agent")).toBeNull();
    expect(fetch.mock.calls.every(([, init]) => !init?.method || init.method === "GET")).toBe(true);
    expect(new Set(fetch.mock.calls.filter(([url]) => /api-keys|service-keys/.test(String(url))).map(([url]) => String(url)))).toEqual(new Set(["/api/cloud/account/api-keys", `${account.api_base.replace("/api/v2", "/api/cloud")}/service-keys`]));
  });

  it("switches compact key tables with active counts and expands effective reach only on request", async () => {
    const lastUsed = new Date(Date.now() - 120_000).toISOString();
    const personal = [
      { ...key, last_used_at: lastUsed, effective_reach: [reach, ...["pending", "blocked", "inactive"].map((status) => ({ ...reach, organization_id: status, name: `${status} org`, status })), { ...reach, organization_id: "empty", name: "Empty org", projects: [] }] },
      { ...key, id: "project", name: "Project agent", access_context: "project", organizations: [{ organization_id: reach.organization_id, project_access: "project", project_ids: [reach.projects[0]!.id] }] },
      { ...key, id: "revoked", name: "Old personal", revoked_at: "2020-01-01T00:00:00Z" },
    ];
    const services = [
      { ...key, id: "service", name: "CI agent", kind: "service", access_context: "selected", organizations: [{ organization_id: reach.organization_id, project_access: "all", project_ids: [] }] },
      { ...key, id: "expired", name: "Old service", kind: "service", expires_at: "2020-01-01T00:00:00Z" },
    ];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      const path = String(url);
      return new Response(JSON.stringify(path.endsWith("key-context") ? context : path.endsWith("key-policy") ? { personal_keys: "allowed" } : { keys: path.endsWith("/account/api-keys") ? personal : path.endsWith("service-keys") ? services : [] }));
    });
    renderSidebarNav("/settings/mcp", client, <SettingsRoute section="mcp" />);
    expect(await screen.findByRole("tab", { name: "Personal 2" })).toHaveProperty("ariaSelected", "true");
    expect(screen.getByRole("tab", { name: "Service 1" })).toBeTruthy();
    const personalTable = within(screen.getByRole("table", { name: "Personal keys" }));
    expect(personalTable.getByText("All my orgs · 5")).toBeTruthy();
    expect(personalTable.getByText(`One project: ${reach.projects[0]!.name}`)).toBeTruthy();
    expect(personalTable.getByText("2m ago").title).toBe(new Date(lastUsed).toLocaleString());
    expect(personalTable.queryByText(/Effective reach today/)).toBeNull();
    const expand = personalTable.getByRole("button", { name: "Expand My agent" });
    fireEvent.click(expand);
    expect(expand.getAttribute("aria-expanded")).toBe("true");
    expect(personalTable.getByText("Effective reach today · All my orgs")).toBeTruthy();
    for (const status of ["Awaiting approval", "Blocked", "Inactive", "No project access"]) expect(personalTable.getByText(status)).toBeTruthy();
    expect(personalTable.getByText(`${reach.projects[0]!.name} (write)`)).toBeTruthy();
    fireEvent.click(personalTable.getByRole("button", { name: "Collapse My agent" }));
    expect(personalTable.queryByText(/Effective reach today/)).toBeNull();
    fireEvent.click(screen.getByRole("tab", { name: "Service 1" }));
    expect(screen.queryByRole("table", { name: "Personal keys" })).toBeNull();
    const serviceTable = within(screen.getByRole("table", { name: "Service keys" }));
    expect(serviceTable.getByText("All org projects")).toBeTruthy();
    expect(screen.getByText(`Service keys act as ${account.organization.name} for CI, bots and team servers. Owners and admins manage them; they keep working when their creator leaves.`)).toBeTruthy();
    expect(serviceTable.queryByText("Old service")).toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "Show revoked and expired" }));
    expect(serviceTable.getByText(/^Expired /)).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Service 1" })).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: "Personal 2" }));
    expect(screen.getByRole("table", { name: "Personal keys" }).textContent).toContain("Old personal");
    fireEvent.click(screen.getByRole("checkbox", { name: "Show revoked and expired" }));
    expect(screen.queryByText("Old personal")).toBeNull();
  });

  it.each([undefined, "personal", "service"] as const)("checks connection identity for %s MCP prompts without changing REST setup", (kind) => {
    const prompt = keySetupPrompt(context.mcp_endpoint, false, kind);
    expect(prompt).toContain("Call connection_info");
    expect(prompt).toContain(kind ? `confirm key.kind is "${kind}", matching the key just created before reporting success` : 'confirm key.kind is "personal" or "service" and explain who it acts as before reporting success');
    const rest = keySetupPrompt(context.mcp_endpoint, true, kind);
    expect(rest).toContain("Authorization: Bearer");
    expect(rest).toContain("https://cloud.example.test/api/v2");
    expect(rest).not.toContain("connection_info");
  });

  it.each(["key", "export", "prompt"])("keeps the key unsaved and shows an inline error when copying %s fails", async (target) => {
    const writeText = vi.fn().mockRejectedValue(new Error("Clipboard denied"));
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => new Response(JSON.stringify(
      String(url).endsWith("key-context") ? context : init?.method === "POST" ? { ...key, token: "private-new-key" } : { keys: [], members: [], invitations: [] },
    )));
    renderSidebarNav("/settings/mcp", client, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Create key" })).toHaveProperty("disabled", false));
    fireEvent.click(screen.getByRole("button", { name: "Create key" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Agent key" } });
    fireEvent.click(screen.getByRole("button", { name: "Create personal key" }));
    await screen.findByLabelText("New API key");
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("button", { name: target === "key" ? "Copy key" : target === "export" ? "Copy as export DETENT_API_KEY=… line" : "Copy MCP setup prompt" }));
    await screen.findByText("Could not copy. Select and save the key privately before closing.");
    expect(dialog.getByText("Copy the key before closing. It cannot be shown again.")).toBeTruthy();
    expect(dialog.queryByLabelText("Step 1 complete")).toBeNull();
    writeText.mockResolvedValue(undefined);
    fireEvent.click(dialog.getByRole("button", { name: "Copy MCP setup prompt" }));
    await waitFor(() => expect(dialog.queryByText("Could not copy. Select and save the key privately before closing.")).toBeNull());
    expect(dialog.getByText("Copy the key before closing. It cannot be shown again.")).toBeTruthy();
    fireEvent.click(dialog.getByRole("button", { name: "Done" }));
  });

  it.each([
    ["personal", "Done", "Example Member"],
    ["personal", "Close", "member@example.test"],
    ["service", "Escape", `${account.organization.name} (service)`],
  ].flatMap(([kind, close, actor]) => ["created", "existing"].map((flow) => [flow, kind, close, actor])))("polls only the %s %s key and stops on %s", async (flow, kind, close, actor) => {
    let lastUsed: string | null = null;
    let pending: ((response: Response) => void) | undefined;
    let defer = false;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      const path = String(url);
      if (path.endsWith("key-context")) return new Response(JSON.stringify(context));
      if (path.endsWith("members")) return new Response(JSON.stringify({ members: [{ id: "membership1", user_id: key.owner, email: key.owner_email, name: close === "Done" ? "Example Member" : "", role: "member", status: "active", grants: [] }], invitations: [] }));
      if (init?.method === "POST") return new Response(JSON.stringify({ ...key, kind, token: "private-new-key" }));
      const ownList = kind === "personal" ? path.endsWith("/account/api-keys") : path.endsWith("/service-keys");
      if (ownList && defer) return new Promise<Response>((resolve) => { pending = resolve; });
      return new Response(JSON.stringify({ keys: ownList ? [{ ...key, id: "other", name: "Other agent", kind, last_used_at: new Date().toISOString() }, { ...key, kind, last_used_at: lastUsed }] : [] }));
    });
    renderSidebarNav("/settings/mcp", client, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Create key" })).toHaveProperty("disabled", false));
    if (flow === "created") {
      fireEvent.click(screen.getByRole("button", { name: "Create key" }));
      if (kind === "service") {
        fireEvent.click(screen.getByRole("radio", { name: "The organization" }));
      }
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Agent key" } });
    }
    vi.useFakeTimers();
    try {
      await act(async () => { fireEvent.click(screen.getByRole("button", { name: flow === "created" ? `Create ${kind} key` : "Connect My agent" })); });
      const dialog = within(screen.getByRole("dialog"));
      expect(dialog.getByText("Waiting for the first call…")).toBeTruthy();
      lastUsed = new Date(Date.now() - 120_000).toISOString();
      await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
      const callLabel = flow === "created" ? /First call/ : /Last call/;
      const first = dialog.getByText(callLabel).textContent;
      expect(first).toContain(`acting as ${actor}`);
      expect(first).toContain("2m ago");
      lastUsed = new Date().toISOString();
      await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
      if (flow === "created") expect(dialog.getByText(callLabel).textContent).toBe(first);
      else expect(dialog.getByText(callLabel).textContent).toContain(`just now · acting as ${actor}`);
      defer = true;
      await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
      expect(pending).toBeDefined();
      await act(async () => {
        if (close === "Escape") fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
        else fireEvent.click(dialog.getByRole("button", { name: close }));
      });
      const reads = fetch.mock.calls.length;
      await act(async () => { pending!(new Response(JSON.stringify({ keys: [{ ...key, kind, last_used_at: lastUsed }] }))); await vi.advanceTimersByTimeAsync(15_000); });
      expect(fetch.mock.calls).toHaveLength(reads);
      expect(screen.queryByLabelText("New API key")).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Create key" }));
      expect(screen.queryByLabelText("New API key")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("redirects the old personal-key URL to the shared page", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => new Response(JSON.stringify(String(url).endsWith("key-context") ? context : String(url).endsWith("key-policy") ? { personal_keys: "allowed" } : { keys: [] })));
    const { router } = renderSidebarNav("/settings/api-keys", client, null, true);
    await screen.findByRole("tab", { name: /^Personal / });
    expect(router.state.location.pathname).toBe("/settings/mcp");
    expect(screen.queryByRole("button", { name: "API keys" })).toBeNull();
  });

  it.each(["personal", "service"])("preserves %s key actions and history through existing endpoints", async (kind) => {
    let revoked = false;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      if (String(url).endsWith("key-context")) return new Response(JSON.stringify(context));
      if (String(url).endsWith("key-policy")) return new Response(JSON.stringify({ personal_keys: "allowed" }));
      if (String(url).endsWith("/rotate")) return new Response(JSON.stringify({ ...key, token: "replacement-key" }), { status: 201 });
      if (init?.method === "DELETE") { revoked = true; return new Response(null, { status: 204 }); }
      const ownList = kind === "personal" ? String(url).endsWith("/account/api-keys") : String(url).endsWith("service-keys");
      return new Response(JSON.stringify({ keys: ownList ? [{ ...key, kind, ...(revoked ? { revoked_at: "2026-10-09T00:00:00Z" } : {}) }] : [] }));
    });
    renderSidebarNav("/settings/api-keys", client, <SettingsRoute section="api-keys" />);
    if (kind === "service") fireEvent.click(await screen.findByRole("tab", { name: "Service 1" }));
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "More actions for My agent" }));
    if (kind === "personal") {
      await user.click(screen.getByRole("menuitem", { name: "Rotate token" }));
      expect(screen.getByRole("alertdialog").textContent).toContain("Rotate My agent?");
      expect(fetch.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
      fireEvent.click(screen.getByRole("button", { name: "Rotate key" }));
      await screen.findByLabelText("New API key");
      fireEvent.click(screen.getByRole("button", { name: "Done" }));
      await user.click(screen.getByRole("button", { name: "More actions for My agent" }));
    } else {
      expect(screen.queryByRole("menuitem", { name: "Rotate token" })).toBeNull();
    }
    await user.click(screen.getByRole("menuitem", { name: "Revoke", exact: true }));
    expect(screen.getByRole("alertdialog").textContent).toContain("Revoke My agent?");
    expect(fetch.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Revoke key" }));
    await screen.findByText(`No active ${kind} keys. Create a key for your agent or integration.`);
    expect(fetch.mock.calls.some(([url, init]) => url === "/api/cloud/account/api-keys/personal1/rotate" && init?.method === "POST")).toBe(kind === "personal");
    expect(fetch.mock.calls.some(([url, init]) => url === (kind === "personal" ? "/api/cloud/account/api-keys/personal1" : `${account.api_base.replace("/api/v2", "/api/cloud")}/service-keys/personal1`) && init?.method === "DELETE")).toBe(true);
    fireEvent.click(screen.getByRole("checkbox", { name: "Show revoked and expired" }));
    expect(screen.getByRole("table", { name: kind === "personal" ? "Personal keys" : "Service keys" }).textContent).toContain("Revoked");
  });

  it("loads the next member-key page and includes its pending keys", async () => {
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      const path = String(url);
      if (path.endsWith("key-context")) return new Response(JSON.stringify(context));
      if (path.endsWith("key-policy")) return new Response(JSON.stringify({ personal_keys: "approval" }));
      if (path.includes("external-keys?cursor=")) return new Response(JSON.stringify({ keys: [{ ...key, id: "personal2", name: "Next page agent", effective_reach: [{ ...reach, status: "pending" }] }] }));
      return new Response(JSON.stringify(path.endsWith("service-keys") ? { keys: [] } : { keys: [key], next_cursor: "personal1" }));
    });
    renderSidebarNav("/settings/mcp", client, <SettingsRoute section="mcp" />);
    fireEvent.click(await screen.findByRole("button", { name: "Load more member keys" }));
    await screen.findByRole("button", { name: "Approve Next page agent" });
    expect(screen.getByRole("button", { name: "Block My agent for this org" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Block Next page agent for this org" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more member keys" })).toBeNull();
    expect(fetch.mock.calls.some(([url]) => String(url).endsWith("/external-keys?cursor=personal1"))).toBe(true);
  });

  it("saves org policy, approves pending keys and revokes only this org", async () => {
    applyHubPaths({ base_path: `/organizations/${account.organization.id}` });
    let policy = "approval";
    let status = "pending";
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      const path = String(url);
      if (path.endsWith("key-context")) return new Response(JSON.stringify(context));
      if (path.endsWith("key-policy")) { if (init?.method === "PUT") policy = JSON.parse(String(init.body)).personal_keys; return new Response(JSON.stringify({ personal_keys: policy })); }
      if (path.endsWith("/approve")) { status = "allowed"; return new Response(null, { status: 204 }); }
      if (path.endsWith("/block")) { status = "blocked"; return new Response(null, { status: 204 }); }
      return new Response(JSON.stringify({ keys: path.endsWith("service-keys") ? [{ ...key, id: "service1", name: "Org integration", kind: "service", access_context: "selected", organizations: [{ organization_id: account.organization.id, project_access: "all", project_ids: null }] }] : [{ ...key, effective_reach: [{ ...reach, status }] }] }));
    });
    renderSidebarNav("/settings/mcp", client, <SettingsRoute section="mcp" />);
    fireEvent.click(await screen.findByRole("tab", { name: "Service 1" }));
    expect(screen.getByRole("button", { name: "More actions for Org integration" })).toBeTruthy();
    expect(screen.getByRole("textbox", { name: "MCP endpoint value" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy MCP setup prompt" })).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Approve My agent" }));
    await screen.findByText("No keys awaiting approval");
    fireEvent.change(screen.getByLabelText("Personal keys"), { target: { value: "blocked" } });
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(policy).toBe("blocked"));
    fireEvent.click(screen.getByRole("button", { name: "Block My agent for this org" }));
    expect(screen.getByText(/The key keeps its access to other organizations/)).toBeTruthy();
    fireEvent.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Block for this org" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(fetch.mock.calls.some(([url, init]) => String(url).endsWith("/external-keys/personal1/block") && init?.method === "PUT")).toBe(true);
    expect(fetch.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(false);
  });
});

describe("the settings navigation in the sidebar", () => {
  beforeEach(() => applyHubPaths({ base_path: "/organizations/org_test" }));
  it.each([true, false])("groups sections for management access %s", async (canManage) => {
    renderSidebarNav("/settings/general", { ...OWNER_ACCOUNT, account: { ...OWNER_ACCOUNT.account, actor: { ...OWNER_ACCOUNT.account.actor, can_manage: canManage } } });
    expect(await screen.findByText(OWNER_ACCOUNT.account.organization.name, { exact: true })).toBeTruthy();
    expect(screen.getByText("Your account", { exact: true })).toBeTruthy();
    await waitFor(() => expect(screen.getByRole("button", { name: "General" })).toBeTruthy());
    for (const label of [
      "General",
      "Organization",
      "Overview",
      "Members",
      "Models",
      "Projects",
      "Providers & runners",
      "Integrations",
      "API keys & MCP",
      "Plan & usage",
      "Billing",
      "Appearance",
      "Keybindings",
      "SnapShots",
      "Source Control",
      "Connections",
      "Archive",
      "What's new",
      "About",
    ]) {
      if (!canManage && ["Plan & usage", "Billing"].includes(label)) expect(screen.queryByRole("button", { name: label })).toBeNull();
      else expect(screen.getByRole("button", { name: label })).toBeTruthy();
    }
  });

  it("marks exactly one row as the section being read", async () => {
    renderSidebarNav("/settings/projects");
    await waitFor(() => expect(screen.getByRole("button", { name: "Projects" })).toBeTruthy());
    const current = screen
      .getAllByRole("button")
      .filter((button) => button.getAttribute("data-active") === "true");
    expect(current).toHaveLength(1);
    expect(current[0]?.textContent).toContain("Projects");
  });

  it("keeps a unavailable section on the list, disabled and explained", async () => {
    renderSidebarNav();
    await waitFor(() => expect(screen.getByRole("button", { name: "Archive" })).toBeTruthy());
    const archive = screen.getByRole("button", { name: "Archive" });
    expect(archive.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByRole("button", { name: "Projects" }).getAttribute("aria-disabled")).toBeNull();

    expect(archive.closest("[data-slot='tooltip-trigger'], span")).toBeTruthy();
  });

  it("refuses to navigate to a disabled section", async () => {
    const { navigated } = renderSidebarNav("/settings/projects");
    await waitFor(() => expect(screen.getByRole("button", { name: "Archive" })).toBeTruthy());
    navigated.length = 0;
    fireEvent.click(screen.getByRole("button", { name: "Archive" }));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(navigated).toEqual([]);
  });

  it("navigates to a section that is served", async () => {
    const { navigated } = renderSidebarNav("/settings/projects");
    await waitFor(() => expect(screen.getByRole("button", { name: "Organization" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Organization" }));
    await waitFor(() => expect(navigated).toContain("/settings/organization"));
  });

  it("hides the sections an actor could only be refused", async () => {
    renderSidebarNav("/settings/general", {
      ...OWNER_ACCOUNT,
      account: {
        ...OWNER_ACCOUNT.account,
        actor: { ...OWNER_ACCOUNT.account.actor, can_manage: false },
      },
    });
    await waitFor(() => expect(screen.getByRole("button", { name: "General" })).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Plan & usage" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Billing" })).toBeNull();
  });

  it("finds a section through the search row", async () => {
    renderSidebarNav();
    const box = await waitFor(() => screen.getByRole("combobox", { name: "Search settings" }));
    fireEvent.change(box, { target: { value: "invoice" } });
    await waitFor(() => expect(screen.getByRole("listbox", { name: "Settings search results" })).toBeTruthy());
    expect(screen.getByRole("option", { name: /Billing/ })).toBeTruthy();
  });

  it("says so when nothing matches", async () => {
    renderSidebarNav();
    const box = await waitFor(() => screen.getByRole("combobox", { name: "Search settings" }));
    fireEvent.change(box, { target: { value: "zzzz" } });
    await waitFor(() => expect(screen.getByText("No settings found")).toBeTruthy());
  });

  it("keeps the way back out of settings", async () => {
    renderSidebarNav();
    await waitFor(() => expect(screen.getByRole("button", { name: "Back" })).toBeTruthy());
  });

  it("renders the brand row exactly once — it is not in this component", async () => {
    renderSidebarNav();
    await waitFor(() => expect(screen.getByRole("button", { name: "General" })).toBeTruthy());
    // `AppSidebarLayout` renders `SidebarChromeHeader` above this nav, so the
    // nav itself must not draw a second one.
    expect(screen.queryByLabelText("Go to Detent Cloud")).toBeNull();
  });
});

describe("the rows a section is built from", () => {
  it("draws the grouped card with its heading and its header action", () => {
    render(
      <SettingsSection title="Projects" headerAction={<button type="button">New project</button>}>
        <SettingsRow title="example-project" description="native · 5 workflow states" status="Set up" />
      </SettingsSection>,
    );
    expect(screen.getByRole("heading", { name: "Projects" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "New project" })).toBeTruthy();
    const row = document.querySelector('[data-slot="settings-row"]');
    expect(row).toBeTruthy();
    expect(within(row as HTMLElement).getByText("native · 5 workflow states")).toBeTruthy();
    expect(within(row as HTMLElement).getByText("Set up")).toBeTruthy();
  });

  it("keeps the direct-row class string, which the parent's separators rely on", () => {
    expect(ITEM_ROW_CLASSNAME).toContain("first:rounded-t-xl");
    expect(ITEM_ROW_CLASSNAME).toContain("last:rounded-b-xl");
  });

  it("clamps a long refusal and offers the rest", () => {
    render(<ExpandableText text={"x".repeat(400)} />);
    const toggle = screen.getByRole("button", { name: "Show full error" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: "Show less" })).toBeTruthy();
  });
});

describe("the providers & runners section", () => {
  it.each([
    {
      name: "two runners with default shared accounts",
      reports: [{ account_alias: "mac-studio" }, { account_alias: "mac-mini" }],
      expected: { accounts: 1, used: 4, max: 12 },
    },
    {
      name: "two runners with the same explicit shared account",
      reports: [{ shared_account_alias: "team", account_alias: "mac-studio" }, { shared_account_alias: "team", account_alias: "mac-mini" }],
      expected: { accounts: 1, used: 4, max: 12 },
    },
    {
      name: "two distinct accounts with the same local alias",
      reports: [{ shared_account_alias: "team-a", used: 2 }, { shared_account_alias: "team-b", used: 3, max_concurrent: 6 }],
      expected: { accounts: 2, used: 5, max: 18 },
    },
    {
      name: "one runner",
      reports: [{}],
      expected: { accounts: 1, used: 4, max: 12 },
    },
    {
      name: "an empty alias joining explicit accounts",
      reports: [{ shared_account_alias: "team-a", used: 2 }, { shared_account_alias: "", used: 7 }, { shared_account_alias: "team-b", used: 5 }],
      expected: { accounts: 1, used: 7, max: 12 },
    },
    {
      name: "an omitted alias joining explicit accounts",
      reports: [{ shared_account_alias: "team-a", used: 2 }, { used: 7 }, { shared_account_alias: "team-b", used: 5 }],
      expected: { accounts: 1, used: 7, max: 12 },
    },
    {
      name: "multiple backends on the same provider pool",
      reports: [{ shared_account_alias: "team" }, { shared_account_alias: "team", backend: "other" }],
      expected: { accounts: 1, used: 4, max: 12 },
    },
    {
      name: "the lowest reported shared pool cap",
      reports: [{ max_concurrent: 12 }, { max_concurrent: 6 }],
      expected: { accounts: 1, used: 4, max: 6 },
    },
    {
      name: "a shared pool without an account limit",
      reports: [{ max_concurrent: undefined }, { max_concurrent: 0 }],
      expected: { accounts: 1, used: 4, max: 0 },
    },
    {
      name: "a bounded shared pool alongside a missing limit",
      reports: [{ max_concurrent: undefined }, { max_concurrent: 12 }],
      expected: { accounts: 1, used: 4, max: 12 },
    },
    {
      name: "a bounded shared pool alongside a zero limit",
      reports: [{ max_concurrent: 0 }, { max_concurrent: 12 }],
      expected: { accounts: 1, used: 4, max: 12 },
    },
  ])("counts pools for $name", ({ reports, expected }) => {
    const runners = reports.map((report, index) => ({
      ...FLEET.runners[0]!,
      id: `runner-${index}`,
      provider_capacity: [{
        provider: "openai", backend: "codex", account_alias: "local",
        models: ["gpt-6.1-sol"], max_concurrent: 12, used: 4,
        availability: "unknown", state: "unknown", observed_at: "2026-10-08T17:13:00Z",
        ...report,
      }],
    }));
    expect(summarizeProviders(runners)).toEqual([expect.objectContaining({ provider: "openai", ...expected })]);
    expect(summarizeProviders([...runners].reverse())).toEqual([expect.objectContaining({ provider: "openai", ...expected })]);
    render(<ProviderSummary runners={runners} />);
    const row = within(screen.getByRole("row", { name: /openai/ }));
    expect(row.getByRole("cell", { name: String(expected.accounts) })).toBeTruthy();
    expect(row.getByText(`${expected.used}/${expected.max}`)).toBeTruthy();
    const progress = row.getByRole("progressbar", { name: "openai in use" });
    expect(progress.getAttribute("aria-valuenow")).toBe(String(expected.used));
    expect(progress.getAttribute("aria-valuemax")).toBe(String(expected.max));
  });

  it("folds every runner's capacity onto one row per provider", () => {
    const rows = summarizeProviders(FLEET.runners);
    expect(rows.map((row) => row.provider).toSorted()).toEqual(["claude", "codex"]);
    const codex = rows.find((row) => row.provider === "codex");
    expect(codex?.accounts).toBeGreaterThan(0);
    expect(codex?.max).toBeGreaterThanOrEqual(codex?.used ?? 0);
    expect(codex?.models.length).toBeGreaterThan(0);
  });

  it("has nothing to fold when no runner reports capacity", () => {
    expect(summarizeProviders([])).toEqual([]);
  });
});

describe("the page container", () => {

  it("mounts inside the router and renders its children", async () => {
    const root = createRootRoute();
    const index = createRoute({
      getParentRoute: () => root,
      path: "/",
      component: () => (
        <SettingsPageContainer>
          <SettingsSection title="Plan & usage">
            <SettingsRow title="pilot_free" />
          </SettingsSection>
        </SettingsPageContainer>
      ),
    });
    const router = createRouter({
      routeTree: root.addChildren([index]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(<RouterProvider router={router as never} />);
    await waitFor(() => expect(screen.getByRole("heading", { name: "Plan & usage" })).toBeTruthy());
    expect(screen.getByText("pilot_free")).toBeTruthy();
  });
});


const usageFixture: BillingUsageReport = {
  organization_id: "org_a",
  renews_at: "2026-11-08T00:00:00Z",
  complimentary: false, expires_at: null, cancels_at: null,
  charged_ai_micros: 31600000,
  chat_usage: {
    range: { from: "2026-10-08T00:00:00Z", to: "2026-11-08T00:00:00Z" },
    input: 4500000, cached_input: 600000, output: 300000, reasoning_output: 0,
    tokens: 4800000, turns: 20, unpriced_turns: 0, cost_usd: 21.07,
  },
  ai_credits: {
    balance_micros: 18400000, auto_enabled: true, threshold_cents: 500, price_id: "credit_25",
    failure: "", in_flight: false, can_auto_fund: false, history: [],
    packs: [{ price_id: "credit_25", label: "$25 credit pack", usd_cents: 2500 }],
  },
  comparison_plans: [
    { id: "starter", version: 1, name: "Starter", monthly_usd_cents: 4900, features: ["hosted_artifacts"], allowances: { concurrent_work: 8, unarchived_issues: 10000, collaboration_bytes: 1073741824 }, price_id: "price_starter" },
    { id: "team", version: 2, name: "Team", monthly_usd_cents: 9900, features: ["hosted_artifacts"], allowances: { concurrent_work: 20, unarchived_issues: 10000, collaboration_bytes: 1073741824 }, price_id: "price_team" },
    { id: "growth", version: 2, name: "Growth", monthly_usd_cents: 24900, features: ["hosted_artifacts"], allowances: { concurrent_work: 50, unarchived_issues: 10000, collaboration_bytes: 1073741824 }, price_id: "price_growth" },
    { id: "scale", version: 2, name: "Scale", features: ["hosted_artifacts"], allowances: { concurrent_work: 100, unarchived_issues: 10000, collaboration_bytes: 1073741824 }, price_id: "" },
  ],
  can_checkout: true, can_manage: true, can_buy_credits: true, checkout_pending: false,
};

const planFixture: PlanReport = {
  organization_id: "org_a", name: "Starter", monthly_usd_cents: 4900,
  base: { id: "starter", version: 1 }, effective_base: { id: "starter", version: 1 },
  source: "subscription", revision: 1, features: ["hosted_artifacts"],
  allowances: { concurrent_work: 8, unarchived_issues: 10000, collaboration_bytes: 1073741824, history_records: 2000000, api_mutations: 20000, ingested_events: 20000 },
  usage: { concurrent_work: 8, unarchived_issues: 1742, collaboration_bytes: 431227904, history_records: 1240000, api_mutations: 3120, ingested_events: 20000 },
  window_ends_at: "2026-10-09T14:00:00Z",
};

function mockPlanUsage(usage: BillingUsageReport = usageFixture, plan: PlanReport = planFixture) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const path = String(input);
    if (path.includes("/billing?view=usage")) return new Response(JSON.stringify(usage));
    if (path.endsWith("/plan")) return new Response(JSON.stringify(plan));
    return new Response(JSON.stringify({ url: "https://checkout.stripe.com/test" }));
  });
}

describe("plan usage meters", () => {
  it.each([
    [79, false],
    [80, true],
    [100, true],
  ])("renders the page alert only when capacity or storage reaches 80%% (%s)", async (used, warning) => {
    const account = OWNER_ACCOUNT.account;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => new Response(JSON.stringify(String(input).includes("/billing?") ? usageFixture : {
      organization_id: account.organization.id,
      name: "Starter",
      monthly_usd_cents: 4900,
      base: { id: "starter", version: 1 },
      effective_base: { id: "starter", version: 1 },
      source: "base",
      revision: 1,
      features: ["hosted_artifacts"],
      allowances: { projects: 100, collaboration_bytes: 1073741824, history_records: 2000000, api_mutations: 20000, ingested_events: 20000 },
      usage: { projects: used, collaboration_bytes: 431227904, history_records: 1240000, api_mutations: 20000, ingested_events: 20000 },
      window_ends_at: "2026-10-09T14:00:00Z",
    })));
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    await screen.findByRole("meter", { name: "Projects" });
    expect(screen.getByRole("meter", { name: "Workspace data" }).getAttribute("aria-valuetext")).toBe("411.25 MB of 1 GB");
    expect(screen.getByRole("meter", { name: "History" }).getAttribute("aria-valuetext")).toBe("1.24M records of 2M records");
    const alert = screen.queryByRole("alert");
    expect(alert !== null).toBe(warning);
    if (alert !== null) {
      expect(alert.textContent).toContain("Projects");
      expect(alert.textContent).not.toContain("API writes");
      expect(alert.textContent).not.toContain("Events received");
    }
    const detailId = screen.getByRole("meter", { name: "API writes" }).getAttribute("aria-describedby")!;
    expect(document.getElementById(detailId)?.textContent).not.toContain("Resets");
    expect(screen.getAllByText(/^Resets /)).toHaveLength(1);
  });

  it.each([
    [431227904, "bytes", "411.25 MB"],
    [1073741824, "bytes", "1 GB"],
    [0, "bytes", "0 B"],
    [1024, "bytes", "1 KB"],
    [1742, "count", "1,742"],
    [1240000, "records", "1.24M records"],
    [2592000, "seconds", "30 days"],
  ] as const)("formats %s as human %s", (value, unit, expected) => {
    expect(formatUsageValue(value, unit)).toBe(expected);
  });

  it.each([
    [79, "Within limit"],
    [80, "Approaching limit"],
    [99, "Approaching limit"],
    [100, "Limit reached"],
    [120, "Limit reached"],
  ])("labels the threshold at %s percent", (used, expected) => {
    render(<UsageMeter label="Projects" used={Number(used)} limit={100} />);
    const meter = screen.getByRole("meter", { name: "Projects" });
    expect(meter.getAttribute("aria-valuenow")).toBe(String(Math.min(Number(used), 100)));
    expect(meter.getAttribute("aria-valuetext")).toBe(`${used} of 100`);
    expect(screen.getByText(new RegExp(String(expected)))).toBeTruthy();
  });

  it("labels an excluded allowance without a reached-limit warning", () => {
    render(<UsageMeter label="Artifact storage" used={0} limit={0} unit="bytes" notIncludedOn="Free" />);
    expect(screen.getByRole("meter", { name: "Artifact storage" }).getAttribute("aria-valuetext")).toBe("Not included on Free");
    expect(screen.queryByText(/Limit reached/)).toBeNull();
  });

  it.each([
    [79, 100, 0],
    [80, 100, 1],
    [100, 100, 1],
    [0, 0, 0],
    [1, 0, 1],
  ])("only selects capacity or storage at the warning threshold (%s/%s)", (used, limit, count) => {
    const plan = {
      allowances: { projects: limit, api_mutations: 100, artifact_retention_seconds: 2592000 },
      usage: { projects: used, api_mutations: 100, artifact_retention_seconds: 2592000 },
    } as unknown as PlanReport;
    const warnings = approachingPlanLimits(plan);
    expect(warnings).toHaveLength(count);
    if (count > 0) expect(warnings[0]?.label).toBe("Projects");
  });
});


describe("plan billing usage", () => {
  it("keeps a newer version of the current tier selectable and recommended", async () => {
    const starter = usageFixture.comparison_plans[0]!;
    const nextStarter = {
      ...starter, version: 2, monthly_usd_cents: 6900,
      allowances: { ...starter.allowances, concurrent_work: 12 },
      price_id: "price_starter_v2",
    };
    const fetch = mockPlanUsage({
      ...usageFixture,
      comparison_plans: [starter, nextStarter, ...usageFixture.comparison_plans.slice(1)],
    });
    const assign = vi.fn();
    vi.stubGlobal("location", { assign, search: "" });
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    const move = await screen.findByRole("button", { name: "Move to Starter" });
    expect(screen.getAllByText("Current")).toHaveLength(1);
    expect(nextFittingPlan(planFixture, [starter, nextStarter])?.price_id).toBe("price_starter_v2");
    expect(move.hasAttribute("disabled")).toBe(false);
    fireEvent.click(move);
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/test"));
    const call = fetch.mock.calls.find(([input]) => String(input).endsWith("/billing/checkout"));
    expect(call).toBeDefined();
    expect(JSON.parse(String((call?.[1] as RequestInit).body)).price).toBe("price_starter_v2");
  });

  it.each(["owner", "admin", "member", "support"])("shows usage and restricts purchases for %s", async (role) => {
    mockPlanUsage();
    const account = OWNER_ACCOUNT.account;
    renderSidebarNav("/settings/plan", { ...OWNER_ACCOUNT, account: {
      ...account, actor: { ...account.actor, role: role === "support" ? "owner" : role },
      support: role === "support" ? { actor: "support@example.test" } : null,
    } }, <PlanSettings />);
    if (role === "member") {
      await screen.findByText("Plan and usage need owner or admin access");
      expect(screen.queryByRole("meter")).toBeNull();
      return;
    }
    await screen.findByText("$31.60 spent this period");
    expect(screen.getByText("$18.40")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Billing" }) !== null).toBe(role === "owner");
    for (const name of ["Upgrade", "Manage billing", "Move to Team", "Move to Growth"]) {
      expect(screen.queryByRole("button", { name }) !== null).toBe(role === "owner");
    }
    expect(screen.queryByRole("button", { name: "Move to Scale" })).toBeNull();
    expect(screen.getByText(/Contact us/)).toBeTruthy();
    expect(screen.getByText("Current")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("Concurrent work");
    expect(screen.getByRole("alert").textContent).not.toContain("Events received");
  });

  it.each([
    ["Move to Growth", "/billing/checkout", "price_growth"],
    ["Manage billing", "/billing/portal", undefined],
  ])("opens the existing hosted flow for %s", async (name, endpoint, price) => {
    const fetch = mockPlanUsage();
    const assign = vi.fn();
    vi.stubGlobal("location", { assign, search: "" });
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    fireEvent.click(await screen.findByRole("button", { name }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/test"));
    const call = fetch.mock.calls.find(([input]) => String(input).endsWith(endpoint));
    expect(call).toBeDefined();
    const request = call?.[1] as RequestInit;
    expect(request.method).toBe("POST");
    const body = JSON.parse(String(request.body));
    expect(body.price).toBe(price);
    expect(body.idempotency_key).toBeTruthy();
  });

  it.each([
    { can_checkout: false, checkout_pending: false },
    { can_checkout: true, checkout_pending: true },
  ])("preserves the existing checkout restriction (%j)", async (restriction) => {
    mockPlanUsage({ ...usageFixture, ...restriction });
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    const button = await screen.findByRole("button", { name: "Move to Growth" });
    expect(button.hasAttribute("disabled")).toBe(true);
  });

  it("moves keyboard focus to the plan comparison", async () => {
    mockPlanUsage();
    const scroll = vi.fn();
    vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(scroll);
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    fireEvent.click(await screen.findByRole("button", { name: "Compare plans" }));
    expect(document.activeElement?.id).toBe("settings-plan-comparison");
    expect(scroll).toHaveBeenCalled();
  });

  it.each([
    [{ complimentary: true, expires_at: "2026-12-31T00:00:00Z" }, "Expires Dec 31, 2026 · 73 days left"],
    [{ complimentary: true, expires_at: null }, "No end date"],
    [{ complimentary: false, renews_at: "2026-11-08T00:00:00Z" }, "Renews Nov 8, 2026"],
    [{ complimentary: false, cancels_at: "2026-11-08T00:00:00Z" }, "Cancels Nov 8, 2026"],
  ])("renders the plan lifecycle (%j)", async (fields, expected) => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-19T00:00:00Z"));
    mockPlanUsage({ ...usageFixture, ...fields });
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    expect(await screen.findByText(expected)).toBeTruthy();
    expect(screen.queryByText("Complimentary") !== null).toBe(fields.complimentary);
  });

  it.each([
    [null, "Custom"],
    [undefined, "Custom"],
    [0, "$0"],
    [4900, "$49"],
  ] as const)("groups excluded allowances and highlights an unoffered current plan priced at %s before the offered tiers", async (price, expectedPrice) => {
    const legacy = { ...planFixture, name: "Legacy complimentary pilot", monthly_usd_cents: price,
      effective_base: { id: "pilot_free", version: 1 }, features: [],
      allowances: { ...planFixture.allowances, relay_bytes: 0, artifact_retained_bytes: 0, artifact_reserved_bytes: 0, artifact_bytes: 0, artifact_upload_bytes: 0, artifact_retention_seconds: 0 },
    };
    mockPlanUsage({ ...usageFixture, name: legacy.name, effective_base: legacy.effective_base, complimentary: true, expires_at: null, renews_at: null }, legacy);
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    await screen.findByText("Complimentary");
    expect(screen.getAllByText("Not on your plan")).toHaveLength(1);
    const excluded = screen.getByText("Not on your plan").parentElement!.parentElement!;
    for (const label of ["Relay data", "Artifact storage", "Reserved artifact storage", "Artifact size", "Upload size", "Artifact retention"]) expect(within(excluded).getByText(label)).toBeTruthy();
    expect(screen.getAllByText(/^Resets /)).toHaveLength(1);
    const comparison = screen.getByLabelText("Compare plans");
    const current = within(comparison).getByText("Current").closest("[aria-current]")!;
    expect(current).toBe(within(comparison).getAllByRole("row")[1]);
    expect(current.className).toContain("ring-info");
    expect(within(current as HTMLElement).getByText(expectedPrice)).toBeTruthy();
    expect(within(current as HTMLElement).getByText("Never expires")).toBeTruthy();
    expect(document.body.textContent).not.toContain("pilot_free");
    expect(within(comparison).getByText("Contact us")).toBeTruthy();
    expect(comparison.textContent).not.toContain("100 slots");
  });

  it("offers Starter from the Free limit banner and preserves the current Free column", async () => {
    const free = { ...usageFixture.comparison_plans[0]!, id: "free", name: "Free", monthly_usd_cents: 0, features: [], price_id: "", allowances: { concurrent_work: 2 } };
    mockPlanUsage({ ...usageFixture, effective_base: { id: "free", version: 1 }, name: "Free", renews_at: null, comparison_plans: [free, ...usageFixture.comparison_plans] }, { ...planFixture, name: "Free", monthly_usd_cents: 0, effective_base: { id: "free", version: 1 }, allowances: { concurrent_work: 2 }, usage: { concurrent_work: 2 } });
    renderSidebarNav("/settings/plan", OWNER_ACCOUNT, <PlanSettings />);
    const banner = await screen.findByRole("alert");
    expect(within(banner).getByText("At your Free limit")).toBeTruthy();
    expect(within(banner).getByRole("button", { name: "Move to Starter" }).hasAttribute("disabled")).toBe(false);
    const current = screen.getByText("Current").closest("[aria-current]")!;
    expect(current.textContent).toContain("Free");
    expect(current.textContent).toContain("Never expires");
    expect(screen.queryByText(/^Renews /)).toBeNull();
  });

  it("does not recommend a tier whose storage has the same limit", () => {
    expect(nextFittingPlan({ ...planFixture, usage: { ...planFixture.usage, collaboration_bytes: 1000000000 } }, usageFixture.comparison_plans)).toBeUndefined();
  });
});


describe("organization settings pages", () => {
  const account = { ...OWNER_ACCOUNT.account, organization: { id: "org_example", name: "Example team" }, organizations: [
    { id: "org_example", name: "Example team", public_url: "", current: true, role: "owner" },
    { id: "org_second", name: "Second team", public_url: "", current: false, role: "member" },
  ] };
  const client = { ...OWNER_ACCOUNT, account, bootstrap: account };
  function stubOrganizationApi(options: { canCreate?: boolean; dialogCanCreate?: boolean; sessionError?: boolean; renameRetry?: boolean; removableMember?: boolean; freeSlotUsed?: boolean } = {}) {
    let sessionReads = 0;
    let renameAttempts = 0;
    let removed = false;
    const reload = vi.fn();
    const assign = vi.fn();
    vi.stubGlobal("location", { reload, assign });
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    const fetcher = vi.fn(async (input: string, init?: RequestInit) => {
      if (input.endsWith("/members")) return Response.json({ roles: membersFixture.roles, member_count: removed ? 2 : 3, members: options.removableMember && !removed ? [{ id: "mem_example", user_id: "user_example", email: "member@example.test", role: "member", status: "active", grants: [] }] : [], invitations: [] });
      if (input.endsWith("/members/mem_example") && init?.method === "DELETE") { removed = true; return Response.json({}); }
      if (input.endsWith("/slug")) return Response.json({ slug: "example-team" });
      if (input.endsWith("/model-selection")) return Response.json({ revision: "1", selection: null, effective: { enabled: true, normal_model: "gpt-6.1-sol" } });
      if (input.endsWith("/name")) { renameAttempts++; return options.renameRetry && renameAttempts === 1 ? Response.json({ code: "unavailable", message: "Try again" }, { status: 503 }) : Response.json({ name: "Renamed team" }); }
      if (input.endsWith("/switch")) return Response.json({ next: "/auth/oidc/start?organization=org_second" });
      if (input.endsWith("/api/v2/organizations") && init?.method === "POST") return Response.json({ organization: { id: "org_new", name: "New team", public_url: "" }, next: "/auth/oidc/start?organization=org_new" });
      if (input === "/api/cloud/session") { sessionReads++; return options.sessionError ? Response.json({ message: "Eligibility unavailable" }, { status: 503 }) : Response.json({ email: "owner@example.test", csrf: "entry-csrf", can_create: sessionReads > 1 ? options.dialogCanCreate ?? options.canCreate ?? true : options.canCreate ?? true, free_slot_used: options.freeSlotUsed ?? true, creation_prices: [{ id: "price_team", label: "Team" }] }); }
      if (input === "/api/cloud/organizations") return Response.json({ email: "owner@example.test", csrf: "entry-csrf", organizations: account.organizations.map((item) => ({ ...item, url: `/organizations/${item.id}` })) });
      if (input === "/organizations" && init?.method === "POST") return Response.json({ next: "/organizations/org_new/provisioning" });
      throw new Error(`Unexpected organization request: ${input}`);
    });
    vi.stubGlobal("fetch", fetcher);
    return { fetcher, reload, assign, writeText };
  }

  it("shows Overview rows, copies the ID, saves the name and switches from the list", async () => {
    const { fetcher, reload, assign, writeText } = stubOrganizationApi();
    renderSidebarNav("/settings/organization", client, null, true);
    await screen.findByRole("heading", { name: "Example team", level: 2 });
    await screen.findByText("You are the owner · 3 members · 2 projects");
    expect(screen.getByText("Organization ID")).toBeTruthy();
    await waitFor(() => expect((screen.getByRole("textbox", { name: "Organization slug" }) as HTMLInputElement).value).toBe("example-team"));
    expect(screen.getByText("Current")).toBeTruthy();
    expect(screen.queryByText("Your first organization")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Copy organization ID" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("org_example"));
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "Organization name" }), { target: { value: " Renamed team " } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    const savedName = fetcher.mock.calls.find(([url]) => url.endsWith("/name"))?.[1];
    expect(savedName?.method).toBe("PUT");
    expect(JSON.parse(savedName!.body as string)).toMatchObject({ name: "Renamed team", idempotency_key: expect.any(String) });
    fireEvent.click(screen.getByRole("button", { name: "Switch to Second team" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/auth/oidc/start?organization=org_second"));
  });

  it("creates only through the dialog and preserves re-sign-in", async () => {
    applyHubPaths({ base_path: "/organizations/org_example" });
    const { assign, fetcher } = stubOrganizationApi({ freeSlotUsed: false });
    renderSidebarNav("/settings/organization", client, null, true);
    fireEvent.click(await screen.findByRole("button", { name: "Create organization" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Organization name" }), { target: { value: "New team" } });
    await waitFor(() => expect((within(dialog).getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create organization" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/organizations/org_new/provisioning"));
    expect(new URLSearchParams(String(fetcher.mock.calls.find(([url]) => url === "/organizations")![1]!.body)).get("name")).toBe("New team");
  });

  it("hides bootstrap-only creation in a provisioned dedicated organization", async () => {
    const { fetcher } = stubOrganizationApi();
    renderSidebarNav("/settings/organization", client, null, true);
    await screen.findByText("You are the owner · 3 members · 2 projects");
    expect(screen.queryByRole("button", { name: "Create organization" })).toBeNull();
    expect(screen.queryByText("New organization")).toBeNull();
    expect(fetcher.mock.calls.some(([url]) => url === "/api/cloud/session" || url === "/api/v2/organizations")).toBe(false);
    expect(screen.getByRole("button", { name: "Switch to Second team" })).toBeTruthy();
  });

  it("requires a paid plan when the shared owner's Free slot is used", async () => {
    applyHubPaths({ base_path: "/organizations/org_example" });
    const { fetcher, assign } = stubOrganizationApi();
    renderSidebarNav("/settings/organization", client, null, true);
    fireEvent.click(await screen.findByRole("button", { name: "Create organization" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Organization name" }), { target: { value: "New team" } });
    await within(dialog).findByText(/Your Free organization slot is used/);
    expect((within(dialog).getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Paid plan" }));
    fireEvent.click(await screen.findByRole("option", { name: "Team" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create organization" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/organizations/org_new/provisioning"));
    expect(String(fetcher.mock.calls.find(([url]) => url === "/organizations")![1]!.body)).toContain("price=price_team");
  });

  it.each([{ canCreate: false }, { sessionError: true }])("hides shared creation when eligibility is unavailable: %o", async (options) => {
    applyHubPaths({ base_path: "/organizations/org_example" });
    const { fetcher } = stubOrganizationApi(options);
    renderSidebarNav("/settings/organization", client, null, true);
    await screen.findByRole("button", { name: "Switch to Second team" });
    await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === "/api/cloud/session")).toBe(true));
    if (options.sessionError) await screen.findByText("Eligibility unavailable");
    expect(screen.queryByRole("button", { name: "Create organization" })).toBeNull();
    expect(screen.queryByText("New organization")).toBeNull();
  });

  it("refuses creation if eligibility changes while opening the dialog", async () => {
    applyHubPaths({ base_path: "/organizations/org_example" });
    const { fetcher, assign } = stubOrganizationApi({ dialogCanCreate: false, freeSlotUsed: false });
    renderSidebarNav("/settings/organization", client, null, true);
    fireEvent.click(await screen.findByRole("button", { name: "Create organization" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Organization name" }), { target: { value: "New team" } });
    await waitFor(() => expect(fetcher.mock.calls.filter(([url]) => url === "/api/cloud/session")).toHaveLength(2));
    expect((within(dialog).getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(within(dialog).getByRole("textbox", { name: "Organization name" }).closest("form")!);
    expect(fetcher.mock.calls.some(([url]) => url === "/organizations")).toBe(false);
    expect(assign).not.toHaveBeenCalled();
  });

  it("reuses the rename key after a failed request", async () => {
    const { fetcher, reload } = stubOrganizationApi({ renameRetry: true });
    renderSidebarNav("/settings/organization", client, null, true);
    fireEvent.change(await screen.findByRole("textbox", { name: "Organization name" }), { target: { value: "Renamed team" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Try again");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    const requests = fetcher.mock.calls.filter(([url]) => url.endsWith("/name"));
    expect(requests).toHaveLength(2);
    expect(requests[1]![1]!.body).toBe(requests[0]![1]!.body);
  });

  it("refreshes sidebar and Overview counts after removing a member", async () => {
    stubOrganizationApi({ removableMember: true });
    const { router } = renderSidebarNav("/settings/organization/members", client, null, true);
    await screen.findByRole("button", { name: "Members 3 members" });
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Actions for member@example.test" }));
    await user.click(await screen.findByRole("menuitem", { name: "Remove from organization" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await screen.findByRole("button", { name: "Members 2 members" });
    expect(screen.queryByText("member@example.test")).toBeNull();
    await router.navigate({ to: "/settings/organization" });
    await screen.findByText("You are the owner · 2 members · 2 projects");
  });

  it.each(["member", "viewer"])("keeps %s views read-only", async (role) => {
    stubOrganizationApi();
    renderSidebarNav("/settings/organization", { ...client, account: { ...account, actor: { ...account.actor, can_manage: false, role } } }, null, true);
    await screen.findByRole("heading", { name: "Example team", level: 2 });
    expect(screen.queryByRole("textbox", { name: "Organization name" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Plan & usage" })).toBeNull();
    expect(screen.getByRole("button", { name: "Copy organization ID" })).toBeTruthy();
  });

  it.each([["/settings/organization/members", "Members", "Pending invitations"], ["/settings/organization/models", "Models", "Model selection"]])("serves the deep link %s and marks its sidebar row", async (path, label, content) => {
    stubOrganizationApi();
    renderSidebarNav(path!, client, null, true);
    await screen.findByText(content!);
    expect(screen.getByRole("button", { name: label === "Members" ? "Members 3 members" : label }).getAttribute("aria-current")).toBe("page");
    expect(screen.queryByText("Your organizations")).toBeNull();
    if (label === "Members") expect(screen.queryByRole("heading", { name: "Model selection" })).toBeNull();
    else expect(screen.queryByRole("heading", { name: "Pending invitations" })).toBeNull();
  });

  it.each([["invitations", "/settings/organization/members"], ["model selection", "/settings/organization/models"]])("searches %s on its new page", async (query, path) => {
    stubOrganizationApi();
    const { router } = renderSidebarNav("/settings/organization", client, null, true);
    fireEvent.change(await screen.findByRole("combobox", { name: "Search settings" }), { target: { value: query } });
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search settings" }), { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toBe(path));
  });
});

const RELEASES = {
  builds: [{
    version: "v1.2.0", build_time: "2026-10-14T12:00:00Z", running: true, patch: false, unread: true,
    entries: [
      { category: "New", text: "Release feed in settings", pr_number: 101, pr_url: "https://github.com/example-org/example-project/pull/101" },
      { category: "Improved", text: "Faster project search", pr_number: 102, pr_url: "https://github.com/example-org/example-project/pull/102" },
      { category: "Fixed", text: "Keep selected filters", pr_number: 103, pr_url: "https://github.com/example-org/example-project/pull/103" },
    ], internal_count: 5,
  }, {
    version: "v1.1.0-op.r.2.g123456789abc", build_time: "2026-10-13T12:00:00Z", running: false, patch: true, unread: false,
    entries: [{ category: "Removed", text: "Retired old integration", pr_number: 100, pr_url: "https://github.com/example-org/example-project/pull/100" }], internal_count: 0,
  }], has_unread: true, next_cursor: "older/+build",
};

describe("What's new", () => {
  it("renders releases with keyboard navigation and category filters, appends the cursor page and marks the running build seen", async () => {
    const user = userEvent.setup();
    let markSeen: ((value: Response) => void) | undefined;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      if (String(url).endsWith("/release-notes/seen")) return await new Promise<Response>((resolve) => { markSeen = resolve; });
      if (String(url).includes("cursor=")) return new Response(JSON.stringify({ builds: [{ ...RELEASES.builds[1], version: "v1.0.0" }], has_unread: false }));
      if (String(url).endsWith("/release-notes")) return new Response(JSON.stringify(RELEASES));
      return new Response(JSON.stringify({ members: [], invitations: [] }));
    });
    renderSidebarNav("/settings/whats-new", { ...OWNER_ACCOUNT, bootstrap: { organization: OWNER_ACCOUNT.account.organization }, account: { ...OWNER_ACCOUNT.account, version: "1.2.0", has_unread: true } }, <SettingsRoute section="whats-new" />);
    await screen.findByText("Running now");
    expect(screen.getByText("Running v1.2.0. Each change links to its pull request.")).toBeTruthy();
    expect(screen.getByText("Patch build")).toBeTruthy();
    expect(screen.getByText("5 internal changes not shown (tests, CI, release tooling)")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Unread release" })).toBeTruthy();
    const versions = screen.getAllByRole("link").filter((link) => link.textContent?.startsWith("v"));
    expect(versions.map((link) => link.textContent)).toEqual(["v1.2.0", "v1.1.0-op.r.2.g123456789abc"]);
    expect(versions[0]?.getAttribute("href")).toBe("https://github.com/digitaldrywood/detent/releases/tag/v1.2.0");
    expect(screen.getByRole("link", { name: "#101" }).getAttribute("href")).toBe("https://github.com/example-org/example-project/pull/101");
    expect(screen.getByRole("link", { name: "#101" }).getAttribute("target")).toBe("_blank");
    act(() => screen.getByRole("button", { name: "About" }).focus());
    const allFilter = screen.getByRole("button", { name: "All" });
    for (let stops = 0; stops < 50 && document.activeElement !== allFilter; stops++) await user.tab();
    expect(document.activeElement).toBe(allFilter);
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "New" }));
    await user.keyboard("{ArrowLeft}");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "All" }));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("link", { name: "v1.2.0" }));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("link", { name: "#101" }));
    expect(screen.getByRole("button", { name: "What's new, new release notes" })).toBeTruthy();
    await waitFor(() => expect(markSeen).toBeTypeOf("function"));
    await act(async () => { markSeen?.(new Response(JSON.stringify({ ...RELEASES, has_unread: false }))); });
    await screen.findByRole("button", { name: "What's new" });
    const writes = fetch.mock.calls.filter(([url]) => String(url).endsWith("/release-notes/seen"));
    expect(writes).toHaveLength(1);
    expect(writes[0]?.[1]?.method).toBe("PUT");
    expect(new Headers(writes[0]?.[1]?.headers).get("X-CSRF-Token")).toBe(accountFixture.csrf_token);
    for (const [category, pr] of [["New", "#101"], ["Improved", "#102"], ["Fixed", "#103"]]) {
      fireEvent.click(screen.getByRole("button", { name: category }));
      expect(screen.getAllByRole("link").filter((link) => link.textContent?.startsWith("#")).map((link) => link.textContent)).toEqual([pr]);
      expect(screen.queryByText("Patch build")).toBeNull();
    }
    fireEvent.click(screen.getByRole("button", { name: "All" }));
    expect(screen.getByText("Removed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Load older builds" }));
    await screen.findByRole("link", { name: "v1.0.0" });
    expect(screen.getByRole("link", { name: "v1.2.0" })).toBeTruthy();
    expect(fetch.mock.calls.some(([url]) => String(url).endsWith("?cursor=older%2F%2Bbuild"))).toBe(true);
    expect(screen.queryByRole("button", { name: "Load older builds" })).toBeNull();
  });

  it("preserves keyboard focus and announces mark-seen retries before clearing unread state on success", async () => {
    const user = userEvent.setup();
    let attempts = 0;
    let completeSeen: ((value: Response) => void) | undefined;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      if (String(url).endsWith("/seen")) {
        if (++attempts === 1) return new Response(JSON.stringify({ code: "unavailable", message: "Please try again." }), { status: 503 });
        return await new Promise<Response>((resolve) => { completeSeen = resolve; });
      }
      if (String(url).includes("release-notes")) return new Response(JSON.stringify(RELEASES));
      return new Response(JSON.stringify({ members: [], invitations: [] }));
    });
    renderSidebarNav("/settings/whats-new", { ...OWNER_ACCOUNT, bootstrap: { organization: OWNER_ACCOUNT.account.organization }, account: { ...OWNER_ACCOUNT.account, has_unread: true } }, <SettingsRoute section="whats-new" />);
    await screen.findByText("Could not mark release notes as seen");
    expect(screen.getByRole("button", { name: "What's new, new release notes" })).toBeTruthy();
    const retry = screen.getByRole("button", { name: "Try again" });
    expect(screen.getByRole("status").textContent).toContain("Please try again.");
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Try again" })).toBe(retry);
    expect(document.activeElement).toBe(retry);
    expect(retry.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByRole("status").textContent).toContain("Marking release notes as seen");
    await user.keyboard("{Enter}");
    expect(attempts).toBe(2);
    await act(async () => { completeSeen?.(new Response(JSON.stringify({ code: "unavailable", message: "Still unavailable." }), { status: 503 })); });
    await screen.findByText("Still unavailable.");
    expect(screen.getByRole("status").textContent).toContain("Still unavailable.");
    expect(document.activeElement).toBe(retry);
    expect(screen.getByRole("button", { name: "What's new, new release notes" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "New" }));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    expect(document.activeElement).toBe(retry);
    await act(async () => { completeSeen?.(new Response(JSON.stringify({ ...RELEASES, has_unread: false }))); });
    await screen.findByRole("button", { name: "What's new" });
    expect(screen.getByRole("status").textContent).toContain("Release notes marked as seen");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "New" }));
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(attempts).toBe(3);
  });

  it.each([true, false])("uses bootstrap unread state (%s) and finds release notes through search", async (has_unread) => {
    const { navigated } = renderSidebarNav("/settings/general", { ...OWNER_ACCOUNT, account: { ...OWNER_ACCOUNT.account, has_unread } });
    const row = await screen.findByRole("button", { name: has_unread ? "What's new, new release notes" : "What's new" });
    expect(row.querySelector('[aria-hidden="true"].bg-primary') !== null).toBe(has_unread);
    fireEvent.change(screen.getByRole("combobox", { name: "Search settings" }), { target: { value: "release notes" } });
    const result = await screen.findByRole("option", { name: /What's new/ });
    fireEvent.click(result);
    await waitFor(() => expect(navigated).toContain("/settings/whats-new"));
  });
});
