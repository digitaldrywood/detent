// @vitest-environment jsdom
import { RegistryProvider } from "@effect/atom-react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it } from "vitest";

import { startMockHub, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { makeRouter } from "../../src/app/router.tsx";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";

Object.defineProperty(globalThis, "scrollTo", { value: () => {}, writable: true });

let hub: MockHub | undefined;
let client: ConversationClient | undefined;

afterEach(async () => {
  cleanup();
  client?.handles.clear();
  client = undefined;
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

async function mountShell(path = "/chat") {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub" });
  await seedConversation(hub.url, "cmd_palette_lease", "Lease renewal under load");
  await seedConversation(hub.url, "cmd_palette_gate", "Explain the admission gate to me");
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({
    origin: hub.url,
    bootstrap,
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

  it("keeps the unavailable commands on the list and announces them disabled", async () => {
    const { router } = await mountShell();
    const popup = await openPalette();
    const before = router.state.location.pathname;

    for (const title of [
      "Go to file",
      "Search project contents",
      "Add project",
      "Toggle theme editor",
    ]) {
      const row = within(popup).getByText(title).closest("[aria-disabled]");
      expect(row, `${title} should render as a disabled row`).not.toBeNull();
      expect(row?.getAttribute("aria-disabled")).toBe("true");
    }

    // A disabled row is inert: clicking it does nothing and the palette stays.
    fireEvent.click(within(popup).getByText("Go to file"));
    expect(router.state.location.pathname).toBe(before);
    expect(screen.queryByTestId("command-palette")).not.toBeNull();
  }, 30_000);
});
