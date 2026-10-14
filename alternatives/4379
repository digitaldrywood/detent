// @vitest-environment jsdom
import { RegistryProvider } from "@effect/atom-react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { startMockHub, type AccountMode, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { isHostedEnvironment } from "../../src/app/components/CommandPalette.tsx";
import { PROJECT_CREATION_UNAVAILABLE } from "../../src/app/projects/NewProject.tsx";
import { HUB_ENVIRONMENT_ID } from "../../src/contracts/index.ts";
import { DETENT_SERVER_CONFIG } from "../../src/state/server.ts";
import { makeRouter } from "../../src/app/router.tsx";
import { lastAccountBootstrap, loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";

Object.defineProperty(globalThis, "scrollTo", { value: () => {}, writable: true });

let hub: MockHub | undefined;
let client: ConversationClient | undefined;

afterEach(async () => {
  cleanup();
  client?.handles.clear();
  client = undefined;
  vi.unstubAllGlobals();
  await hub?.close();
  hub = undefined;
});

/** See the note in `app.test.tsx`: undici rejects jsdom's `AbortSignal`. */
const sameRealmFetch: typeof globalThis.fetch = (input, init) => {
  const { signal: _abort, ...rest } = (init ?? {}) as RequestInit;
  return globalThis.fetch(input as string, rest);
};

async function seedConversation(hubUrl: string, key: string, text: string): Promise<void> {
  const response = await fetch(
    `${hubUrl}/api/v2/organizations/org_mock/projects/proj_alpha/conversations`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        key,
        first_message: { key: `${key}_msg`, text },
      }),
    },
  );
  if (!response.ok) throw new Error(`seed failed: ${response.status}`);
}

async function mountShell(path = "/chat", options: { readonly account?: AccountMode; readonly platformRole?: string } = {}) {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub" });
  await seedConversation(hub.url, "cmd_palette_lease", "Lease renewal under load");
  await seedConversation(hub.url, "cmd_palette_gate", "Explain the admission gate to me");
  if (options.account !== undefined) {
    await fetch(`${hub.url}/__mock/account`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: options.account }),
    });
  }
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({
    origin: hub.url,
    bootstrap,
    account: lastAccountBootstrap() === null ? null : {
      ...lastAccountBootstrap()!,
      actor: { ...lastAccountBootstrap()!.actor, platform_role: options.platformRole },
    },
    transport: fetchEventStreamTransport(sameRealmFetch),
    heartbeatTimeoutMs: 20_000,
  });
  const router = makeRouter(createMemoryHistory({ initialEntries: [path] }));
  render(
    <RegistryProvider>
      <ClientContext.Provider value={client}>
        <RouterProvider router={router} />
      </ClientContext.Provider>
    </RegistryProvider>,
  );
  // The shell is up once the sidebar has listed what the hub was seeded with:
  // the palette reads the same list, so waiting for the row is waiting for it.
  await screen.findByLabelText("Search threads", undefined, { timeout: 10_000 });
  await screen.findByText("Lease renewal under load", undefined, { timeout: 10_000 });
  return { router };
}

function pressModK(): void {
  fireEvent.keyDown(window, { key: "k", metaKey: true });
  fireEvent.keyDown(window, { key: "k", ctrlKey: true });
}

function palette(): HTMLElement {
  return screen.getByTestId("command-palette");
}

async function openPalette(): Promise<HTMLElement> {
  pressModK();
  return await screen.findByTestId("command-palette", undefined, { timeout: 5_000 });
}

function paletteInput(): HTMLInputElement {
  return within(palette()).getByRole("combobox") as HTMLInputElement;
}

describe("the command palette", () => {
  it.each(["admin", "support", "billing", "viewer", "", undefined])("offers the platform command only with a platform role (%s)", async (platformRole) => {
    await mountShell("/chat", { platformRole });
    const assign = vi.fn();
    vi.stubGlobal("location", { assign });
    const dialog = await openPalette();
    fireEvent.change(paletteInput(), { target: { value: "Platform console" } });
    if (platformRole) {
      const command = await within(dialog).findByRole("option", { name: /Platform console/ });
      fireEvent.click(command);
      await waitFor(() => expect(assign).toHaveBeenCalledWith("/platform/tenants"));
    } else {
      await waitFor(() => expect(within(dialog).queryByRole("option", { name: /Platform console/ })).toBeNull());
    }
  });

  it("opens on Mod+K and closes on the same shortcut", async () => {
    await mountShell();

    const popup = await openPalette();
    expect(popup.getAttribute("aria-label")).toBe("Command palette");

    expect(within(popup).getByText("Actions")).toBeTruthy();
    expect(within(popup).getByText("Navigate")).toBeTruthy();

    pressModK();
    await waitFor(() => expect(screen.queryByTestId("command-palette")).toBeNull(), {
      timeout: 5_000,
    });
  }, 30_000);

  it("filters conversations and destinations as the reader types", async () => {
    await mountShell();
    const popup = await openPalette();

    expect(within(popup).getByText("Recent Threads")).toBeTruthy();

    fireEvent.change(paletteInput(), { target: { value: "Lease renewal" } });
    await waitFor(
      () => {
        expect(within(palette()).getByText("Lease renewal under load")).toBeTruthy();
      },
      { timeout: 5_000 },
    );
    expect(within(palette()).queryByText("Explain the admission gate to me")).toBeNull();

    // A destination is reachable by its own name, not only a conversation.
    fireEvent.change(paletteInput(), { target: { value: "usage" } });
    await waitFor(
      () => {
        expect(within(palette()).getByText("Usage")).toBeTruthy();
      },
      { timeout: 5_000 },
    );
    expect(within(palette()).queryByText("Lease renewal under load")).toBeNull();
  }, 30_000);

  it("navigates to the highlighted row on Enter", async () => {
    const { router } = await mountShell();
    await openPalette();

    const input = paletteInput();
    fireEvent.change(input, { target: { value: "Lease renewal" } });
    await waitFor(
      () => expect(within(palette()).getByText("Lease renewal under load")).toBeTruthy(),
      { timeout: 5_000 },
    );
    fireEvent.keyDown(input, { key: "Enter" });

    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 5_000,
    });
    await waitFor(() => expect(screen.queryByTestId("command-palette")).toBeNull(), {
      timeout: 5_000,
    });
  }, 30_000);

  it("closes on Escape and gives focus back to where it came from", async () => {
    await mountShell();
    const search = screen.getByLabelText<HTMLInputElement>("Search threads");
    search.focus();
    expect(document.activeElement).toBe(search);

    await openPalette();
    // The palette takes focus: typing cannot leak into the sidebar behind it.
    await waitFor(() => expect(document.activeElement).toBe(paletteInput()), { timeout: 5_000 });

    fireEvent.keyDown(paletteInput(), { key: "Escape" });
    await waitFor(() => expect(screen.queryByTestId("command-palette")).toBeNull(), {
      timeout: 5_000,
    });
    await waitFor(() => expect(document.activeElement).toBe(search), { timeout: 5_000 });
  }, 30_000);

  it("offers the workspace's own commands only where there is a workspace", async () => {
    await mountShell();

    // On a chat route the workspace publishes its commands upward
    // (`adapters/paletteContext.ts`) and the palette offers them.
    const popup = await openPalette();
    expect(within(popup).getByText("Copy link")).toBeTruthy();
    expect(within(popup).getByText("Toggle right panel")).toBeTruthy();
    // The window's own toggle comes from the sidebar provider instead, so it
    // is offered on every route.
    expect(within(popup).getByText("Toggle sidebar")).toBeTruthy();

    // Usage has no workspace, so the rows are withdrawn rather than offering
    // to copy a link to a conversation that is no longer on screen.
    fireEvent.change(paletteInput(), { target: { value: "usage" } });
    await waitFor(() => expect(within(palette()).getByText("Usage")).toBeTruthy(), {
      timeout: 5_000,
    });
    fireEvent.keyDown(paletteInput(), { key: "Enter" });
    await waitFor(() => expect(screen.queryByTestId("command-palette")).toBeNull(), {
      timeout: 5_000,
    });

    const onUsage = await openPalette();
    expect(within(onUsage).queryByText("Copy link")).toBeNull();
    expect(within(onUsage).queryByText("Toggle right panel")).toBeNull();
    expect(within(onUsage).getByText("Toggle sidebar")).toBeTruthy();
  }, 30_000);

  it("opens the New project dialog in place from Add project", async () => {
    const { router } = await mountShell();
    const before = router.state.location.pathname;
    const popup = await openPalette();
    const row = within(popup).getByText("Add project").closest("[aria-disabled]");
    expect(row?.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(within(popup).getByText("Add project"));

    const dialog = await screen.findByRole("dialog", { name: "New project" }, { timeout: 5_000 });
    expect(within(dialog).getByLabelText("Name")).toBeTruthy();
    expect(router.state.location.pathname).toBe(before);
    await waitFor(() => expect(screen.queryByTestId("command-palette")).toBeNull(), {
      timeout: 5_000,
    });
  }, 30_000);

  it("says why a reader who cannot manage projects cannot add one", async () => {
    await mountShell("/chat", { account: "read_only" });
    const popup = await openPalette();
    const row = within(popup).getByText("Add project").closest("[aria-disabled]");
    expect(row?.getAttribute("aria-disabled")).toBe("true");
    expect(within(popup).getByText(PROJECT_CREATION_UNAVAILABLE)).toBeTruthy();

    fireEvent.click(within(popup).getByText("Add project"));
    expect(screen.queryByRole("dialog", { name: "New project" })).toBeNull();
    expect(screen.queryByTestId("command-palette")).not.toBeNull();
  }, 30_000);

  it("hides the checkout commands a hosted project can never run", async () => {
    const { router } = await mountShell();
    const popup = await openPalette();
    const before = router.state.location.pathname;

    expect(within(popup).queryByText("Go to file")).toBeNull();
    expect(within(popup).queryByText("Search project contents")).toBeNull();

    fireEvent.change(paletteInput(), { target: { value: "grep" } });
    await waitFor(
      () => expect(within(palette()).queryByText("Search project contents")).toBeNull(),
      { timeout: 5_000 },
    );

    fireEvent.change(paletteInput(), { target: { value: "" } });
    const row = await waitFor(() => {
      const found = within(palette()).getByText("Toggle theme editor").closest("[aria-disabled]");
      expect(found?.getAttribute("aria-disabled")).toBe("true");
      return found;
    });
    fireEvent.click(row as Element);
    expect(router.state.location.pathname).toBe(before);
    expect(screen.queryByTestId("command-palette")).not.toBeNull();
  }, 30_000);
});

describe("isHostedEnvironment", () => {
  it.each([
    { machine: "cloud", hosted: true },
    { machine: "server", hosted: false },
    { machine: undefined, hosted: false },
  ] as const)("reads machine $machine as hosted=$hosted", ({ machine, hosted }) => {
    const presentation = {
      environmentId: HUB_ENVIRONMENT_ID,
      label: "Example",
      displayUrl: null,
      relayManaged: false,
      serverConfig: {
        ...DETENT_SERVER_CONFIG,
        environment: {
          capabilities: DETENT_SERVER_CONFIG.environment.capabilities,
          ...(machine === undefined ? {} : { machine }),
        },
      },
    };
    expect(isHostedEnvironment(presentation)).toBe(hosted);
  });

  it("does not treat a missing environment as hosted", () => {
    expect(isHostedEnvironment(null)).toBe(false);
  });
});
