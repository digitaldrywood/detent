// @vitest-environment jsdom
//
// One end-to-end pass through the shell against the mock hub: a chat becomes a
// linked issue, a runner picks it up, and a question is answered. It exists to
// catch wiring the component tests cannot see — the route that must not change
// when a conversation is linked, and the controls that must carry the attempt
// the strip is showing.
import { RegistryProvider } from "@effect/atom-react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { startMockHub, type CoordinatorMode, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext, LAST_PROJECT_STORAGE_KEY } from "../../src/app/client.ts";
import type { Bootstrap } from "../../src/contracts/index.ts";
import { makeRouter } from "../../src/app/router.tsx";
import { setComposerText } from "./composerInput.ts";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";
import * as sharedEntry from "../../src/app/entry/shared.ts";
import { useUiStateStore } from "../../src/uiStateStore.ts";
import * as humanActions from "../../src/app/adapters/sidebarHumanActions.ts";
import { sidebarProjectScopeKey } from "../../src/app/adapters/shell.ts";
import { DEFAULT_VIEW_STATE } from "../../src/app/work/lib/viewState.ts";
import "../../src/app/reports/ReportsPage.tsx";
import "../../src/app/ChatRoutes.tsx";

// The router restores scroll on navigation; jsdom has no scrolling.
Object.defineProperty(globalThis, "scrollTo", { value: () => {}, writable: true });

let hub: MockHub | undefined;
let client: ConversationClient | undefined;
const nativeFetch = globalThis.fetch;

beforeEach(() => {
  globalThis.localStorage.clear();
  useUiStateStore.setState({ sidebarProjectScopeKey: null, sidebarProjectWorkingSetKeys: [], sidebarProjectScopeAccountKey: null, sidebarProjectScopesByAccount: {} });
  vi.stubGlobal("fetch", sameRealmFetch);
});

afterEach(async () => {
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  cleanup();
  client?.handles.clear();
  client = undefined;
  await hub?.close();
  hub = undefined;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/**
 * jsdom installs its own `AbortController` while `fetch` stays Node's, and
 * undici rejects a signal from the other realm. A browser has one realm and
 * uses `EventSource` besides, so this is a test-environment artifact: drop the
 * signal here rather than weaken the transport. The stream is closed when the
 * hub shuts down at the end of the test.
 */
const sameRealmFetch: typeof globalThis.fetch = (input, init) => {
  const { signal: _abort, ...rest } = (init ?? {}) as RequestInit;
  return nativeFetch(input as string, rest);
};

function holdResponse(method: string, pathname: string) {
  let receive = () => {};
  let release = () => {};
  const received = new Promise<void>((resolve) => { receive = resolve; });
  const released = new Promise<void>((resolve) => { release = resolve; });
  const fetch = globalThis.fetch;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const response = await fetch(input, init);
    if (new URL(String(input)).pathname === pathname && (init?.method ?? "GET") === method) {
      await response.clone().arrayBuffer();
      receive();
      await released;
    }
    return response;
  });
  return { received, release };
}

interface MountOptions {
  readonly bootstrap?: (bootstrap: Bootstrap) => Bootstrap;
  readonly storedProject?: string;
  readonly coordinator?: CoordinatorMode;
  /** Flipped on the hub after any setup, before the client boots. */
  readonly readOnly?: boolean;
  /** Resolved after `seed`, so a test can open a chat the seed created. */
  readonly path?: string | (() => string);
  /** Runs against the hub before the client boots, e.g. to seed a chat. */
  readonly seed?: (hubUrl: string) => Promise<void>;
}

async function mountApp(options: MountOptions | CoordinatorMode = {}) {
  const settings: MountOptions = typeof options === "string" ? { coordinator: options } : options;
  hub = await startMockHub({
    deltaDelayMs: 0,
    heartbeatMs: 5_000,
    coordinator: settings.coordinator ?? "hub",
  });
  await settings.seed?.(hub.url);
  if (settings.readOnly === true) {
    await fetch(`${hub.url}/__mock/account`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: "read_only" }),
    });
  }
  const loaded = await loadBootstrap(hub.url);
  const bootstrap = settings.bootstrap?.(loaded) ?? loaded;
  const streamRequests: URL[] = [];
  const streamFetch: typeof globalThis.fetch = (input, init) => {
    streamRequests.push(new URL(input instanceof Request ? input.url : String(input)));
    return sameRealmFetch(input, init);
  };
  client = makeClient({
    origin: hub.url,
    bootstrap,
    transport: fetchEventStreamTransport(streamFetch),
    heartbeatTimeoutMs: 20_000,
  });
  if (settings.storedProject !== undefined) {
    globalThis.localStorage.setItem(LAST_PROJECT_STORAGE_KEY, settings.storedProject);
  }
  const path =
    typeof settings.path === "function" ? settings.path() : (settings.path ?? "/chat");
  const router = makeRouter(createMemoryHistory({ initialEntries: [path] }));
  await router.load();
  render(
    <RegistryProvider>
      <ClientContext.Provider value={client}>
        <RouterProvider router={router} />
      </ClientContext.Provider>
    </RegistryProvider>,
  );
  return { router, hub, client, streamRequests };
}

async function control(path: string, body?: unknown): Promise<void> {
  await fetch(`${hub!.url}/__mock/${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body ?? {}),
  });
}

describe("the conversation shell", () => {
  it.each(["/work", "/work/p/proj_alpha", "/alpha"])("filters the working set locally from %s and after returning from Chat", async (path) => {
    const actions = ["alpha", "beta"].map((name) => ({ id: `wi_${name}`, projectId: `proj_${name}`, label: `${name} question`, to: `/work/i/wi_${name}`, opened_at: "2026-10-20T10:00:00Z" }));
    vi.spyOn(humanActions, "useSidebarHumanActions").mockReturnValue(actions);
    const requests: URL[] = [];
    let pendingBoardRequests = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://fixture.test");
      requests.push(url);
      if (url.pathname === "/api/v2/organizations/org_mock/readable/alpha") {
        return Promise.resolve(Response.json({ project_id: "proj_alpha", work_item_id: "" }));
      }
      const board = url.pathname.endsWith("/work-items") && url.searchParams.get("include") === "board";
      if (board) pendingBoardRequests++;
      try {
        const response = await sameRealmFetch(input, init);
        if (board) await response.clone().arrayBuffer();
        return response;
      } finally {
        if (board) pendingBoardRequests--;
      }
    });
    const { router } = await mountApp({ path, bootstrap: (bootstrap) => {
      const keys = bootstrap.projects.filter((project) => ["proj_alpha", "proj_beta"].includes(project.id)).map(sidebarProjectScopeKey);
      useUiStateStore.setState({ sidebarProjectWorkingSetKeys: keys });
      return bootstrap;
    } });
    const rows = () => within(screen.getByRole("list", { name: "Project working set" }));
    await waitFor(() => expect(screen.getAllByTestId("issue-card").length).toBeGreaterThan(0));
    const heading = () => screen.getByRole("heading", { level: 1 });
    expect(heading().textContent).toBe(path === "/work" ? "2 projects" : "alpha");
    const boardRequests = () => requests.filter((url) => url.pathname.endsWith("/work-items") && url.searchParams.get("include") === "board").length;
    const initial = boardRequests();
    const boardControl = screen.getByTestId("board-new-issue");
    expect(initial).toBeGreaterThan(0);
    fireEvent.click(rows().getByRole("button", { name: "All" }));
    await waitFor(() => expect(heading().textContent).toBe("2 projects"));
    expect(router.state.location.pathname).toBe(path);
    expect(screen.getByTestId("board-new-issue")).toBe(boardControl);
    expect(boardRequests()).toBe(initial);
    act(() => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab" }));
      within(heading()).getByText("2 projects").focus();
    });
    expect((await screen.findByRole("tooltip")).textContent).toBe("alpha, beta");
    const allCards = screen.getAllByTestId("issue-card").map((card) => card.textContent);
    expect(screen.getByTestId("attention-issue-wi_alpha")).toBeTruthy();
    expect(screen.getByTestId("attention-issue-wi_beta")).toBeTruthy();
    for (const name of ["alpha", "beta"]) {
      fireEvent.click(rows().getByRole("button", { name }));
      await waitFor(() => expect(heading().textContent).toBe(name));
      expect(screen.getByTestId(`attention-issue-wi_${name}`)).toBeTruthy();
      expect(screen.queryByTestId(`attention-issue-wi_${name === "alpha" ? "beta" : "alpha"}`)).toBeNull();
      expect(screen.getAllByTestId("issue-card").length).toBeLessThan(allCards.length);
      expect(screen.queryByTestId("project-dot")).toBeNull();
      expect(boardRequests()).toBe(initial);
    }
    fireEvent.click(rows().getByRole("button", { name: "All" }));
    await waitFor(() => expect(heading().textContent).toBe("2 projects"));
    expect(screen.getAllByTestId("issue-card").map((card) => card.textContent)).toEqual(allCards);
    expect(boardRequests()).toBe(initial);
    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    await waitFor(() => expect(screen.getByRole("table")).toBeTruthy());
    const listRequests = boardRequests();
    const allList = within(screen.getByRole("table")).getAllByRole("row").length;
    for (const name of ["alpha", "beta"]) {
      fireEvent.click(rows().getByRole("button", { name }));
      await waitFor(() => expect(heading().textContent).toBe(name));
      expect(within(screen.getByRole("table")).getAllByRole("row").length).toBeLessThan(allList);
      expect(boardRequests()).toBe(listRequests);
    }
    fireEvent.click(screen.getByTestId("nav-chat"));
    await waitFor(() => expect(router.state.location.pathname).toBe("/chat"));
    expect(rows().getByRole("button", { name: "beta" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByLabelText("Filter threads by project").textContent).toContain("2 of");
    await screen.findByLabelText("Message");
    fireEvent.click(screen.getByTestId("nav-work"));
    await waitFor(() => expect(router.state.location.pathname).toBe("/work/p/proj_beta"));
    fireEvent.click(await screen.findByRole("radio", { name: "Board" }));
    await waitFor(() => expect(screen.getAllByTestId("issue-card").length).toBeGreaterThan(0));
    await waitFor(() => expect(heading().textContent).toBe("beta"));
    await waitFor(() => expect(boardRequests()).toBeGreaterThan(listRequests));
    await waitFor(() => expect(pendingBoardRequests).toBe(0));
    const returnedRequests = boardRequests();
    const returnedControl = screen.getByTestId("board-new-issue");
    for (const name of ["All", "alpha", "beta"]) {
      fireEvent.click(rows().getByRole("button", { name }));
      await waitFor(() => expect(heading().textContent).toBe(name === "All" ? "2 projects" : name));
      expect(screen.getByTestId("board-new-issue")).toBe(returnedControl);
      expect(boardRequests()).toBe(returnedRequests);
      for (const project of ["alpha", "beta"]) {
        expect(screen.queryByTestId(`attention-issue-wi_${project}`) !== null).toBe(name === "All" || name === project);
      }
    }
  });

  it.each([false, true])("restores saved-view project scope throughout the shell (direct entry: %s)", async (direct) => {
    const savedViews = holdResponse("GET", "/api/v2/organizations/org_mock/work-views");
    const { router } = await mountApp({
      storedProject: "proj_alpha",
      path: direct ? "/work/views/view_1" : "/work/p/proj_alpha",
      seed: async (origin) => {
        const base = `${origin}/api/v2/organizations/org_mock`;
        for (const [project, title] of [["proj_alpha", "Alpha scope thread"], ["proj_beta", "Beta scope thread"]]) {
          await fetch(`${base}/projects/${project}/conversations`, {
            method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ key: project, title }),
          });
        }
        for (const [name, project_id] of [["Beta saved view", "proj_beta"], ["All saved view", null]]) {
          await fetch(`${base}/work-views`, {
            method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name, project_id, scope: "personal", favorite: true, state: DEFAULT_VIEW_STATE }),
          });
        }
      },
    });
    await act(async () => {
      await savedViews.received;
      expect(screen.queryByRole("button", { name: "Beta saved view" })).toBeNull();
      savedViews.release();
    });
    if (!direct) {
      await waitFor(() => expect(screen.getByRole("button", { name: "alpha" }).getAttribute("aria-pressed")).toBe("true"));
      fireEvent.click(await screen.findByRole("button", { name: "Beta saved view" }));
    }
    await waitFor(() => expect(screen.getByRole("button", { name: "beta" }).getAttribute("aria-pressed")).toBe("true"));
    expect(router.state.location.pathname).toBe("/work/views/view_1");
    expect(globalThis.localStorage.getItem(LAST_PROJECT_STORAGE_KEY)).toBe("proj_beta");
    expect(await screen.findByText("Beta scope thread")).toBeTruthy();
    expect(screen.queryByText("Alpha scope thread")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Work" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/work/p/proj_beta"));
    await act(async () => { router.history.back(); });
    await waitFor(() => expect(router.state.location.pathname).toBe("/work/views/view_1"));
    fireEvent.click(screen.getByRole("button", { name: "All saved view" }));
    await waitFor(() => expect(screen.getByLabelText("Filter threads by project").textContent).toContain("All Mock organization projects"));
    expect(globalThis.localStorage.getItem(LAST_PROJECT_STORAGE_KEY)).toBe("");
    expect(await screen.findByText("Alpha scope thread")).toBeTruthy();
    expect(screen.getByText("Beta scope thread")).toBeTruthy();
    await act(async () => { router.history.back(); });
    await waitFor(() => expect(screen.getByRole("button", { name: "beta" }).getAttribute("aria-pressed")).toBe("true"));
    await act(async () => { router.history.forward(); });
    await waitFor(() => expect(screen.getByLabelText("Filter threads by project").textContent).toContain("All Mock organization projects"));
    fireEvent.click(screen.getByRole("button", { name: "Work" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/work"));
  });

  it("keeps a shared Reports view in sync with sidebar project changes", async () => {
    const { router } = await mountApp({ path: "/reports/flow?project=proj_alpha&range=30d&compare=off", storedProject: "proj_alpha" });
    await screen.findByRole("heading", { name: "Reports" });
    fireEvent.click(await screen.findByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "beta" }));
    fireEvent.keyDown(screen.getByLabelText("Search projects"), { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "beta" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ project: "proj_beta", range: "30d", compare: "off" }));
    expect(router.state.location.pathname).toBe("/reports/flow");
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("button", { name: "Clear" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ project: "all", range: "30d", compare: "off" }));
    expect(await screen.findByText("Choose a project in the sidebar to view its reports.")).toBeTruthy();
  });

  it("copies an exact read-only conversation handoff without posting or dispatching", async () => {
    let conversationId = "";
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", new Proxy(navigator, {
      get: (target, key) => key === "clipboard" ? { writeText } : Reflect.get(target, key, target),
    }));
    const fetchWithLists = globalThis.fetch;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const response = await fetchWithLists(input, init);
      if (new URL(String(input)).pathname === "/api/v2/organizations/org_mock/conversations") {
        return Response.json({ conversations: [], next_cursor: null });
      }
      return response;
    });
    const { hub, client } = await mountApp({
      readOnly: true,
      storedProject: "proj_beta",
      seed: async (hubUrl) => {
        const response = await fetch(`${hubUrl}/api/v2/organizations/org_mock/projects/proj_alpha/conversations`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ key: "seed-handoff", title: "Private handoff" }),
        });
        conversationId = (await response.json()).conversation.id;
      },
      path: () => `/chat/c/${conversationId}?project_id=proj_alpha&panel=files#transient`,
    });
    const endpoint = `${hub.url}/api/v2/organizations/org_mock/projects/proj_alpha/conversations/${conversationId}`;
    const before = await (await fetch(endpoint)).json();
    const openThreadActions = async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Thread actions for Private handoff" }, { timeout: 5000 }));
      await act(async () => {
        await new Promise<void>((resolve) => window.requestAnimationFrame(() => resolve()));
      });
    };
    const copy = async (name: string) => {
      await openThreadActions();
      fireEvent.click(await screen.findByRole("button", { name }));
      await waitFor(() => expect(writeText).toHaveBeenCalled());
    };
    await copy("Copy link");
    const url = `${globalThis.location.origin}/chat/c/${conversationId}?project_id=proj_alpha`;
    expect(writeText).toHaveBeenLastCalledWith(url);
    writeText.mockClear();
    await copy("Copy conversation ID");
    expect(writeText).toHaveBeenLastCalledWith(conversationId);
    writeText.mockClear();
    await copy("Copy for local AI");
    const handoff = writeText.mock.calls[0]![0] as string;
    expect(handoff).toContain(JSON.stringify({ organization_id: "org_mock", project_id: "proj_alpha", conversation_id: conversationId, web_url: url }, null, 2));
    expect(handoff).toContain("list_conversation_messages");
    expect(handoff).toContain("get_conversation_attachment");
    expect(handoff).toContain("access_denied");
    expect(handoff).not.toContain("transient");
    expect(handoff).not.toContain(client.bootstrap.csrf_token);
    expect(await (await fetch(endpoint)).json()).toEqual(before);
    writeText.mockRejectedValueOnce(new Error("denied"));
    await openThreadActions();
    fireEvent.click(await screen.findByRole("button", { name: "Copy link" }));
    expect(await screen.findByText("Could not copy to clipboard")).toBeTruthy();
  });

  it("switches sidebar search scope without mixing Work and Chat queries", async () => {
    const { router } = await mountApp({ path: "/work?q=invitation" });
    const search = await screen.findByRole<HTMLInputElement>("searchbox", { name: "Search issues" });
    await waitFor(() => expect(search.value).toBe("invitation"));
    expect(screen.getAllByRole("searchbox")).toHaveLength(1);
    expect(search.placeholder).toBe("Search issues");
    expect(screen.queryByRole("listbox", { name: "Thread search results" })).toBeNull();
    fireEvent.keyDown(document.body, { key: "/" });
    expect(document.activeElement).toBe(search);
    fireEvent.change(search, { target: { value: "lease" } });
    await waitFor(() => expect(new URLSearchParams(router.state.location.searchStr).get("q")).toBe("lease"));
    await act(async () => { await router.navigate({ to: "/chat" }); });
    const chat = await screen.findByRole<HTMLInputElement>("combobox", { name: "Search threads" });
    expect(chat.value).toBe("");
    expect(chat.placeholder).toBe("Search chats");
    fireEvent.change(chat, { target: { value: "a chat title" } });
    expect(screen.getByText("No threads found")).toBeTruthy();
    await act(async () => { await router.navigate({ to: "/work/p/$projectId", params: { projectId: "proj_alpha" }, search: { q: "checkout" } }); });
    await waitFor(() => expect(screen.getByRole<HTMLInputElement>("searchbox", { name: "Search issues" }).value).toBe("checkout"));
    expect(screen.queryByText("No threads found")).toBeNull();
    expect(screen.queryByRole("listbox", { name: "Thread search results" })).toBeNull();
  });

  it.each([
    ["", "proj_alpha", "alpha"],
    ["proj_stale", "proj_alpha", "alpha"],
    ["proj_readonly", "proj_alpha", "alpha"],
    ["proj_beta", "proj_beta", "beta"],
  ])("starts a writable chat with stored project %j", async (stored, expectedId, expectedName) => {
    const created = holdResponse("POST", `/api/v2/organizations/org_mock/projects/${expectedId}/conversations`);
    const { router, hub } = await mountApp({
      storedProject: stored,
      bootstrap: (bootstrap) => ({
        ...bootstrap,
        projects: [...bootstrap.projects].sort((a, b) => Number(a.can_write) - Number(b.can_write)),
      }),
    });
    const composer = await screen.findByLabelText<HTMLElement>("Message");
    expect(screen.getByTestId("hero-headline").textContent).toContain(expectedName);
    expect(screen.queryByText("Project unknown")).toBeNull();
    expect(screen.queryByText("Choose a project first")).toBeNull();
    await setComposerText(composer, "Start a writable chat");
    expect(screen.getByLabelText<HTMLButtonElement>("Send message").disabled).toBe(false);
    await act(async () => {
      fireEvent.keyDown(composer, { key: "Enter" });
      await created.received;
      expect(router.state.location.pathname).toBe("/chat");
      created.release();
    });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/));
    const conversationId = router.state.location.pathname.replace("/chat/c/", "");
    const response = await fetch(`${hub.url}/api/v2/organizations/org_mock/projects/${expectedId}/conversations/${conversationId}`);
    expect(response.status).toBe(200);
    expect((await response.json()).conversation.project_id).toBe(expectedId);
  });

  it("remembers the last real project through Work and All projects", async () => {
    const { router } = await mountApp({ path: "/chat/p/proj_beta" });
    await screen.findByLabelText("Message");
    await waitFor(() => expect(globalThis.localStorage.getItem(LAST_PROJECT_STORAGE_KEY)).toBe("proj_beta"));
    await router.navigate({ to: "/work" });
    await waitFor(() => expect(screen.getByRole("button", { name: "beta" }).getAttribute("aria-pressed")).toBe("true"));
    expect(globalThis.localStorage.getItem(LAST_PROJECT_STORAGE_KEY)).toBe("proj_beta");
    await router.navigate({ to: "/chat" });
    await screen.findByLabelText("Message");
    expect(screen.getByTestId("hero-headline").textContent).toContain("beta");
    await router.navigate({ to: "/chat/p/$projectId", params: { projectId: "proj_beta" } });
    await waitFor(() => expect(screen.getByRole("button", { name: "beta" }).getAttribute("aria-pressed")).toBe("true"));
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("button", { name: "Clear" }));
    await waitFor(() => expect(screen.getByLabelText("Filter threads by project").textContent).toContain("All Mock organization projects"));
    expect(globalThis.localStorage.getItem(LAST_PROJECT_STORAGE_KEY)).toBe("");
    const composer = screen.getByLabelText<HTMLElement>("Message");
    await setComposerText(composer, "Chat after All projects");
    expect(screen.getByLabelText<HTMLButtonElement>("Send message").disabled).toBe(false);
    expect(screen.getByTestId("hero-headline").textContent).toContain("beta");
  });

  it.each([
    ["proj_alpha", "alpha", false],
    ["proj_readonly", "readonly", true],
  ])("keeps the explicit chat project %s", async (projectId, projectName, readOnly) => {
    await mountApp({ path: `/chat/p/${projectId}`, storedProject: "proj_beta" });
    const composer = await screen.findByLabelText<HTMLElement>("Message");
    expect(screen.getByTestId("hero-headline").textContent).toContain(projectName);
    await setComposerText(composer, "Use the explicit project");
    if (readOnly) {
      expect(screen.getByText("Read-only project")).toBeTruthy();
      expect(screen.queryByLabelText("Send message")).toBeNull();
    } else {
      expect(screen.getByLabelText<HTMLButtonElement>("Send message").disabled).toBe(false);
    }
  });

  it.each([false, true])("switches the footer workspace through the authenticated owner (shared entry: %s)", async (shared) => {
    const assign = vi.fn();
    vi.stubGlobal("location", { assign, pathname: "/work", search: "", hash: "" });
    vi.spyOn(sharedEntry, "behindSharedEntry").mockReturnValue(shared);
    vi.spyOn(sharedEntry, "useSharedOrganizations").mockReturnValue({ value: shared ? [
      { id: "org_mock", name: "Mock organization", url: "/organizations/org_mock", current: true },
      { id: "org_second", name: "Second mock organization", url: "/organizations/org_second", current: false },
    ] : null, error: null, loading: false, refresh: async () => {}, set: () => {} });
    const switches: unknown[] = [];
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith("/switch")) switches.push(JSON.parse(String(init?.body)));
      return sameRealmFetch(input, init);
    });
    await mountApp({ path: "/work" });
    const trigger = await screen.findByRole("button", { name: "Switch workspace: Mock organization" });
    const toolbar = trigger.closest("ul")!;
    expect([...toolbar.querySelectorAll("button")].slice(0, 4).map((button) => button.getAttribute("aria-label"))).toEqual([
      "Switch workspace: Mock organization", "Settings", "Pull Requests", "Usage",
    ]);
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mock organization" }));
    expect(assign).not.toHaveBeenCalled();
    expect(switches).toEqual([]);
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Second mock organization" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(shared ? "/organizations/org_second" : "https://second.mock.test/auth/oidc/start"));
    if (shared) expect(switches).toEqual([]);
    else expect(switches).toEqual([{ organization: "org_second", idempotency_key: expect.any(String) }]);
  });

  it("keeps an open chat visible and reconnects after a server_error close", async () => {
    const { router, streamRequests } = await mountApp();
    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, { timeout: 5_000 });
    await setComposerText(composer, "Keep this chat visible");
    await waitFor(() => expect(screen.getByRole<HTMLButtonElement>("button", { name: "Send message" }).disabled).toBe(false));
    fireEvent.keyDown(composer, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), { timeout: 5_000 });
    await screen.findByTestId("assistant-turn", undefined, { timeout: 5_000 });
    const chatPath = router.state.location.pathname;
    const userTurn = screen.getByTestId("user-turn");
    const conversationId = chatPath.replace("/chat/c/", "");
    const subscriptions = () => streamRequests.filter((url) => url.pathname.endsWith(`/${conversationId}/events`));
    await waitFor(() => expect(subscriptions()).toHaveLength(1));
    const cursor = subscriptions()[0]!.searchParams.get("after");
    expect(Number(cursor)).toBeGreaterThan(0);

    await control("drop-open-streams", { reason: "server_error" });
    await screen.findByText(/The live connection dropped\. Reconnecting/, undefined, { timeout: 5_000 });
    expect(screen.getByTestId("user-turn")).toBe(userTurn);
    expect(screen.queryByText("Conversation unavailable")).toBeNull();
    expect(router.state.location.pathname).toBe(chatPath);
    await waitFor(() => expect(subscriptions()).toHaveLength(2), { timeout: 5_000 });
    expect(subscriptions()[1]!.searchParams.get("after")).toBe(cursor);

    await setComposerText(screen.getByLabelText<HTMLElement>("Message"), "Still connected");
    await waitFor(() => expect(screen.getByRole<HTMLButtonElement>("button", { name: "Send message" }).disabled).toBe(false));
    fireEvent.keyDown(screen.getByLabelText("Message"), { key: "Enter" });
    await waitFor(() => expect(screen.getAllByTestId("user-turn").some((turn) => turn.textContent?.includes("Still connected"))).toBe(true), { timeout: 5_000 });
    await waitFor(() => expect(screen.queryByText(/The live connection dropped\. Reconnecting/)).toBeNull(), { timeout: 5_000 });
    expect(screen.getAllByTestId("user-turn")[0]).toBe(userTurn);
    expect(userTurn.textContent).toContain("Keep this chat visible");
    expect(router.state.location.pathname).toBe(chatPath);
    expect(screen.queryByText("Conversation unavailable")).toBeNull();
  });

  it("carries a chat through handoff, a runner and a question", async () => {
    const { router } = await mountApp();
    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });

    await setComposerText(composer, "Please create an issue for the lock renewal");
    await waitFor(() => expect(screen.getByRole<HTMLButtonElement>("button", { name: "Send message" }).disabled).toBe(false));
    fireEvent.keyDown(composer, { key: "Enter" });

    // The conversation opened on its own route and the transcript is live.
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 5_000,
    });
    const chatPath = router.state.location.pathname;
    const conversationId = chatPath.replace("/chat/c/", "");

    // The coordinator proposes an issue; the card prefills the handoff form.
    const proposal = await screen.findByTestId("proposal-card", undefined, { timeout: 5_000 });
    expect(proposal.textContent).toContain("Please create an issue for the lock renewal");

    expect(screen.getAllByRole("button", { name: "Create linked issue" })).toHaveLength(1);
    expect(screen.getByLabelText("Project actions")).not.toBeNull();
    fireEvent.click(within(proposal).getByRole("button", { name: "Create linked issue" }));

    const form = await screen.findByTestId("handoff-form");
    expect(form.textContent).toContain(
      "messages in this chat become readable by everyone who can read project alpha",
    );
    fireEvent.change(within(form).getByLabelText("Objective"), {
      target: { value: "Preserve the lock renewal objective" },
    });
    fireEvent.change(within(form).getByLabelText("Priority"), {
      target: { value: "1" },
    });
    fireEvent.click(within(form).getByRole("button", { name: "Create linked issue" }));

    // Linking moves the reader to the issue: the issue is the page now and the
    // conversation is a surface on it, so `/chat/c/:id`
    // for a linked chat redirects to `/work/i/:workItem?panel=conversation`
    // and the transcript comes with it, in the right panel.
    await waitFor(
      () => expect(router.state.location.pathname).toMatch(/^\/work\/i\//),
      { timeout: 5_000 },
    );
    expect(router.state.location.pathname).not.toBe(chatPath);
    const workItemId = router.state.location.pathname.replace("/work/i/", "");
    const linkedIssue = await fetch(
      `${hub!.url}/api/v2/organizations/org_mock/work-items/${workItemId}`,
    ).then((response) => response.json());
    expect(linkedIssue.body).toBe("Preserve the lock renewal objective");
    expect(linkedIssue.priority).toBe(1);

    await waitFor(
      () =>
        expect(router.state.location.search).toMatchObject({ panel: "conversation" }),
      { timeout: 5_000 },
    );
    // The result card arrives as a message on the stream, just behind the
    // response that flipped the resource.
    const card = await screen.findByTestId("issue-card", undefined, { timeout: 5_000 });
    expect(card.textContent).toContain("Waiting for a runner");
    // The history the reader had before the link is still the history.
    expect(screen.getAllByTestId("user-turn")[0]?.textContent).toContain(
      "Please create an issue for the lock renewal",
    );
    expect(screen.getByTestId("execution-copy").textContent).toContain(
      "The issue is queued. Nothing is running yet.",
    );

    // A runner picks the issue up.
    // The strip is one line, so a status whose sentence only restates its
    // label prints the label alone (Michael's review, September 11).
    await control(`runner/${conversationId}/start`);
    await waitFor(() => expect(screen.getByTestId("execution-copy").textContent).toBe("Running"), {
      timeout: 5_000,
    });
    expect(screen.getByTestId("execution-attempt").textContent).toBe("attempt att_1");
    // The strip is the one place the issue's state is said; the composer's
    // footer no longer repeats it as a scope sentence (Michael, September 12).
    expect(screen.queryByTestId("composer-scope")).toBeNull();

    await control(`runner/${conversationId}/question`);
    const option = await screen.findByRole("button", { name: /Half the lease/ }, { timeout: 5_000 });
    fireEvent.click(option);
    fireEvent.submit(option.closest("form") as HTMLFormElement);

    // The answer lands, the options go, and the transcript keeps the record so
    // a second reader finds the resolution rather than a second form.
    await waitFor(
      () => expect(screen.getByTestId("stored-answer").textContent).toContain("Half the lease"),
      { timeout: 5_000 },
    );
    expect(screen.queryByRole("button", { name: /Half the lease/ })).toBeNull();
    expect(screen.getByTestId("question-card").getAttribute("data-locked")).toBe("true");
  }, 30_000);

  // The runner-dispatched coordinator through the whole shell: the strip is the
  // only thing that tells a reader whether an unlinked chat is queued, being
  // answered, or over, and the Stop it offers has to reach the hub as `cancel`.
  it("shows an unlinked chat waiting for a runner, then answering, then stopped", async () => {
    const { router } = await mountApp("runner");
    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    await setComposerText(composer, "Why does the lease lapse?");
    fireEvent.keyDown(composer, { key: "Enter" });

    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 5_000,
    });
    const conversationId = router.state.location.pathname.replace("/chat/c/", "");

    // Waiting for a runner names the project a runner has to be enrolled in.
    await waitFor(
      () =>
        expect(screen.getByTestId("execution-copy").textContent).toContain(
          "Waiting for a runner to pick up this chat in alpha.",
        ),
      { timeout: 5_000 },
    );
    // It is a chat, not an issue: no issue controls, and nothing to configure.
    expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(screen.getByTestId("delivery-label").textContent).toBe("Queued for a runner");

    await control(`runner/${conversationId}/start`);
    await waitFor(() => expect(screen.getByTestId("execution-copy").textContent).toBe("Running"), {
      timeout: 5_000,
    });

    // Stop is `cancel`, and the ladder ends where the strip says it does.
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    await waitFor(() => expect(screen.getByTestId("execution-copy").textContent).toBe("Stopped"), {
      timeout: 5_000,
    });
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  }, 30_000);

  it("says no runner can answer chats yet without taking the send away", async () => {
    const { router } = await mountApp("none");
    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });

    const note = await screen.findByTestId("coordinator-note");
    expect(note.textContent).toContain("No runner can answer chats in this project yet.");
    // The one create action is still there, and so is the send: the message
    // queues and waits rather than being refused.
    expect(screen.getAllByRole("button", { name: "New thread" })).toHaveLength(1);
    expect(screen.getByLabelText<HTMLButtonElement>("Send message").disabled).toBe(true);

    await setComposerText(composer, "Anyone home?");
    expect(screen.getByLabelText<HTMLButtonElement>("Send message").disabled).toBe(false);
    fireEvent.keyDown(composer, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 5_000,
    });
    const turn = await screen.findByTestId("user-turn", undefined, { timeout: 5_000 });
    expect(turn.textContent).toContain("Anyone home?");
  }, 30_000);

  // The shortcuts are wired in the shell, so this is the only place that can
  // see that `/` reaches the real search box and that `Mod+Shift+N` calls the
  // one new-chat action rather than a second one of its own.
  it("answers the two keyboard shortcuts from anywhere in the shell", async () => {
    const { router } = await mountApp();
    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });

    const search = screen.getByRole("combobox", { name: "Search threads" });

    // `/` while the reader is typing is a slash, not a shortcut.
    composer.focus();
    fireEvent.keyDown(composer, { key: "/" });
    expect(document.activeElement).toBe(composer);

    // `/` anywhere else focuses the sidebar search.
    document.body.focus();
    fireEvent.keyDown(document.body, { key: "/" });
    expect(document.activeElement).toBe(search);

    expect(screen.getAllByRole("button", { name: "New thread" })).toHaveLength(1);

    // Send one message so the shell is on a conversation route to leave.
    await setComposerText(composer, "Where does the lease lapse?");
    fireEvent.keyDown(composer, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 5_000,
    });

    // `Mod+Shift+N` starts a new chat in the current project context, from
    // inside the composer, where a bare letter key would be typing.
    const docked = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    docked.focus();
    fireEvent.keyDown(docked, { key: "N", ctrlKey: true, shiftKey: true });
    await waitFor(() => expect(router.state.location.pathname).toBe("/chat"), { timeout: 5_000 });
    expect(await screen.findByTestId("hero-headline")).toBeDefined();
  }, 30_000);

  // A viewer reads the chat and is offered nothing that would write.
  it("gives a read-only viewer the transcript and none of the controls", async () => {
    let conversationId = "";
    await mountApp({
      readOnly: true,
      seed: async (hubUrl) => {
        const response = await fetch(
          `${hubUrl}/api/v2/organizations/org_mock/projects/proj_alpha/conversations`,
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              key: "cmd_seed_readonly",
              first_message: {
                key: "cmd_seed_readonly_msg",
                text: "Why does the lease lapse?",
              },
            }),
          },
        );
        conversationId = ((await response.json()) as { conversation: { id: string } })
          .conversation.id;
      },
      path: () => `/chat/c/${conversationId}`,
    });

    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    await waitFor(() => expect(screen.getAllByTestId("user-turn").length).toBeGreaterThan(0), {
      timeout: 5_000,
    });
    expect(composer.getAttribute("contenteditable")).toBe("false");
    expect(screen.getByText("You can read this chat but not send messages")).toBeTruthy();
    expect(screen.queryByLabelText("Stop the current turn")).toBeNull();
    expect(screen.queryByRole("button", { name: "Create linked issue" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
    expect(screen.queryByTestId("conversation-menu-button")).toBeNull();
  }, 30_000);

  // Settled replaced Archive, and the client offers no archive action,
  // no banner and no closed composer — a settled chat is one you can still
  // write in, and writing in it is what unsettles it.
  it("offers no archive action, and a settled chat stays writable", async () => {
    const { router } = await mountApp();
    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    await setComposerText(composer, "Put this away later");
    fireEvent.keyDown(composer, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 5_000,
    });
    const conversationId = router.state.location.pathname.replace("/chat/c/", "");

    expect(screen.queryByTestId("conversation-menu-button")).toBeNull();
    expect(screen.queryByTestId("archived-banner")).toBeNull();

    // The hub settles it; the client keeps the composer open.
    await control("settle", { conversation: conversationId, settled: true });
    await control(`runner/${conversationId}/start`);
    await waitFor(() =>
      expect(
        screen.getByLabelText<HTMLElement>("Message").getAttribute("contenteditable"),
      ).toBe("true"),
    );
  }, 30_000);

  // A snapshot that cannot be read says so and offers the one action that
  // might fix it, instead of an endless "Opening conversation…".
  it("reports a conversation it could not open and offers a retry", async () => {
    await mountApp({ path: "/chat/c/conv_does_not_exist" });
    const error = await screen.findByTestId("snapshot-error", undefined, { timeout: 5_000 });
    expect(error.textContent).toContain("This conversation could not be opened.");
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  }, 30_000);
});
