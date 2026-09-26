// @vitest-environment jsdom
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

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
import { summarizeProviders } from "../../src/app/fleet/RunnersSection.tsx";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import type { FleetResponse } from "../../src/contracts/account.ts";

afterEach(cleanup);

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

  it("names every section it can route to", () => {
    for (const id of Object.keys(SETTINGS_SECTION_LABELS)) {
      expect(isSettingsSectionId(id)).toBe(true);
    }
    expect(isSettingsSectionId("appearance")).toBe(false);
    expect(isSettingsSectionId(DEFAULT_SECTION)).toBe(true);
  });
});

function renderSidebarNav(pathname = "/settings/projects", account: unknown = OWNER_ACCOUNT) {
  const navigated: string[] = [];
  const root = createRootRoute();
  const section = createRoute({
    getParentRoute: () => root,
    path: "/settings/$section",
    component: () => (
      <SidebarProvider>
        <SettingsSidebarNav pathname={pathname} />
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
  account: { actor: { can_manage: true }, support: null },
};

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
    renderSidebarNav("/settings/general", { account: { actor: { can_manage: false }, support: null } });
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
