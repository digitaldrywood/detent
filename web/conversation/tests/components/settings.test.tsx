// @vitest-environment jsdom
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as Schema from "effect/Schema";

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
import { GeneralSettings, SettingsRoute } from "../../src/app/settings/Settings.tsx";
import { organizationMCPEndpoint } from "../../src/app/settings/MCPSettings.tsx";
import { applyHubPaths, resetHubPaths } from "../../src/runtime/basePath.ts";

afterEach(() => {
  cleanup();
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
  it("gives an owner every Detent section, in the reading order", () => {
    const items = settingsNavItems({ canManage: true, supporting: false });
    expect(labelsFor(items)).toEqual([
      "General",
      "Organization",
      "Appearance",
      "Projects",
      "Providers & runners",
      "Integrations",
      "API & MCP",
      "Plan",
      "Billing",
      "Keybindings",
      "SnapShots",
      "Source Control",
      "Connections",
      "Archive",
      "About",
    ]);
  });

  it("hides plan and billing from somebody who could only be refused them", () => {
    const items = settingsNavItems({ canManage: false, supporting: false });
    expect(labelsFor(items)).not.toContain("Plan");
    expect(labelsFor(items)).not.toContain("Billing");
  });

  it("closes billing during a support session, and leaves the plan open", () => {
    const items = settingsNavItems({ canManage: true, supporting: true });
    expect(labelsFor(items)).toContain("Plan");
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

  it("names every section it can route to", () => {
    for (const id of Object.keys(SETTINGS_SECTION_LABELS)) {
      expect(isSettingsSectionId(id)).toBe(true);
    }
    expect(isSettingsSectionId("appearance")).toBe(false);
    expect(isSettingsSectionId(DEFAULT_SECTION)).toBe(true);
  });
});

function renderSidebarNav(pathname = "/settings/projects", account: unknown = OWNER_ACCOUNT, content: React.ReactNode = null) {
  const navigated: string[] = [];
  const root = createRootRoute();
  const section = createRoute({
    getParentRoute: () => root,
    path: "/settings/$section",
    component: () => (
      <SidebarProvider>
        <SettingsSidebarNav pathname={pathname} />
        {content}
      </SidebarProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([section]),
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
  return { navigated, subscribe };
}

const OWNER_ACCOUNT = {
  http: { origin: "", apiBase: accountFixture.api_base, csrfToken: accountFixture.csrf_token },
  account: Schema.decodeUnknownSync(AccountBootstrap)(accountFixture),
};

describe("MCP setup", () => {
  const account = accountFixture as unknown as AccountBootstrap;

  it.each([
    ["dedicated", "", "https://threefold.detent.cloud/", "https://threefold.detent.cloud/mcp"],
    ["shared origin", "/organizations/org_threefold", "https://app.detent.cloud", "https://app.detent.cloud/organizations/org_threefold/mcp"],
    ["shared entry", "/organizations/org_threefold", "https://app.detent.cloud/organizations/org_threefold", "https://app.detent.cloud/organizations/org_threefold/mcp"],
    ["shared trailing slash", "/organizations/org_threefold/", "https://app.detent.cloud/organizations/org_threefold/", "https://app.detent.cloud/organizations/org_threefold/mcp"],
    ["mounted public URL without runtime mount", "", "https://app.detent.cloud/organizations/org_threefold", "https://app.detent.cloud/organizations/org_threefold/mcp"],
    ["another organization's URL", "/organizations/org_threefold", "https://app.detent.cloud/organizations/org_parable", null],
    ["another organization's URL without runtime mount", "", "https://app.detent.cloud/organizations/org_parable", null],
    ["another organization's mount", "/organizations/org_parable", "https://app.detent.cloud", null],
    ["conflicting public path", "/organizations/org_threefold", "https://app.detent.cloud/projects/proj_parable", null],
    ["HTTP", "", "http://threefold.detent.cloud", null],
    ["credentials in URL", "/organizations/org_threefold", "https://user:secret@threefold.detent.cloud", null],
    ["query credential", "/organizations/org_threefold", "https://threefold.detent.cloud?token=secret", null],
    ["fragment", "/organizations/org_threefold", "https://threefold.detent.cloud#secret", null],
    ["malformed", "", "not a URL", null],
  ])("uses only a safe canonical %s organization endpoint", (_name, mount, publicURL, expected) => {
    applyHubPaths({ base_path: mount });
    const decoded = Schema.decodeUnknownSync(AccountBootstrap)({ ...account, organization: { ...account.organization, public_url: publicURL }, organizations: [] });
    expect(organizationMCPEndpoint(decoded)).toBe(expected);
  });

  it("matches the active organization's identity rather than another directory entry", () => {
    expect(organizationMCPEndpoint({ ...account, organizations: [...account.organizations].reverse() })).toBe("https://threefold.detent.cloud/mcp");
    expect(organizationMCPEndpoint({ ...account, organization: { id: "org_unknown", name: "Unknown" } })).toBeNull();
    expect(organizationMCPEndpoint(null)).toBeNull();
  });

  it.each([
    ["", "https://threefold.detent.cloud", "https://threefold.detent.cloud/mcp"],
    ["/organizations/org_threefold", "https://app.detent.cloud", "https://app.detent.cloud/organizations/org_threefold/mcp"],
    ["/organizations/org_threefold", "https://app.detent.cloud/organizations/org_threefold", "https://app.detent.cloud/organizations/org_threefold/mcp"],
    ["/organizations/org_threefold", "https://app.detent.cloud/organizations/org_parable", null],
  ])("routes a viewer to setup at %j and copies only a matching organization URL or placeholder examples using shared key metadata", async (mount, publicURL, expected) => {
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
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: "/api/v2/organizations/org_threefold", csrfToken: activeAccount.csrf_token }, account: activeAccount, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
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
    fireEvent.click(screen.getByText("Connect to your own Detent daemon"));
    fireEvent.click(screen.getByRole("button", { name: "Copy Local stdio configuration" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(expected ? 4 : 1));
    expect(JSON.parse(writeText.mock.lastCall![0]).mcpServers.detent.env.DETENT_API_TOKEN).toBe("YOUR_DETENT_API_TOKEN");
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
    renderSidebarNav("/settings/mcp", { http: { origin: "", apiBase: "/api/v2/organizations/org_threefold", csrfToken: account.csrf_token }, account, bootstrap: { organization: account.organization } }, <SettingsRoute section="mcp" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Copy MCP endpoint" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Copy MCP endpoint" }));
    await waitFor(() => expect(screen.getByRole("status", { name: "MCP endpoint copy status" }).textContent).toContain("Select the text"));
  });
});

describe("the settings navigation in the sidebar", () => {
  it("lists every section the actor gets, in the reading order", async () => {
    renderSidebarNav();
    await waitFor(() => expect(screen.getByRole("button", { name: "General" })).toBeTruthy());
    for (const label of [
      "General",
      "Organization",
      "Appearance",
      "Projects",
      "Providers & runners",
      "Integrations",
      "API & MCP",
      "Plan",
      "Billing",
      "Keybindings",
      "SnapShots",
      "Source Control",
      "Connections",
      "Archive",
      "About",
    ]) {
      expect(screen.getByRole("button", { name: label })).toBeTruthy();
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
    expect(screen.queryByRole("button", { name: "Plan" })).toBeNull();
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
        <SettingsRow title="parable" description="native · 5 workflow states" status="Set up" />
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
          <SettingsSection title="Plan">
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
    await waitFor(() => expect(screen.getByRole("heading", { name: "Plan" })).toBeTruthy());
    expect(screen.getByText("pilot_free")).toBeTruthy();
  });
});
