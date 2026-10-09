// @vitest-environment jsdom
//
// One end-to-end pass through the shell against the mock hub: a chat becomes a
// linked issue, a runner picks it up, and a question is answered. It exists to
// catch wiring the component tests cannot see — the route that must not change
// when a conversation is linked, and the controls that must carry the attempt
// the strip is showing.
import { RegistryProvider } from "@effect/atom-react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

// The router restores scroll on navigation; jsdom has no scrolling.
Object.defineProperty(globalThis, "scrollTo", { value: () => {}, writable: true });

let hub: MockHub | undefined;
let client: ConversationClient | undefined;
const nativeFetch = globalThis.fetch;

beforeEach(() => {
  globalThis.localStorage.clear();
  vi.stubGlobal("fetch", sameRealmFetch);
});

afterEach(async () => {
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

interface MountOptions {
  readonly bootstrap?: (bootstrap: Bootstrap) => Bootstrap;
  readonly storedProject?: string;
  readonly coordinator?: CoordinatorMode;
  /** Flipped on the hub after any setup, before the client boots (§10.11). */
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
  it.each([
    ["", "proj_alpha", "alpha"],
    ["proj_stale", "proj_alpha", "alpha"],
    ["proj_readonly", "proj_alpha", "alpha"],
    ["proj_beta", "proj_beta", "beta"],
  ])("starts a writable chat with stored project %j", async (stored, expectedId, expectedName) => {
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
    fireEvent.keyDown(composer, { key: "Enter" });
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
    await waitFor(() => expect(screen.getByLabelText("Filter threads by project").textContent).toContain("All projects"));
    expect(globalThis.localStorage.getItem(LAST_PROJECT_STORAGE_KEY)).toBe("");
    await router.navigate({ to: "/chat" });
    await screen.findByLabelText("Message");
    expect(screen.getByTestId("hero-headline").textContent).toContain("beta");
    await router.navigate({ to: "/chat/p/$projectId", params: { projectId: "proj_beta" } });
    await waitFor(() => expect(screen.getByLabelText("Filter threads by project").textContent).toContain("beta"));
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "All projects" }));
    await waitFor(() => expect(screen.getByLabelText("Filter threads by project").textContent).toContain("All projects"));
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
    vi.spyOn(sharedEntry, "useSharedOrganizations").mockReturnValue(shared ? [
      { id: "org_mock", name: "Mock organization", url: "/organizations/org_mock", current: true },
      { id: "org_second", name: "Second mock organization", url: "/organizations/org_second", current: false },
    ] : null);
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
    fireEvent.click(
      screen.getByLabelText("Share this conversation's history with the project"),
    );
    fireEvent.click(within(form).getByRole("button", { name: "Create linked issue" }));

    // Linking moves the reader to the issue: the issue is the page now and the
    // conversation is a surface on it (decisions.md §19.4), so `/chat/c/:id`
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
    // a second reader finds the resolution rather than a second form (§5).
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
    // queues and waits rather than being refused (decisions.md §9.1).
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
  // one new-chat action rather than a second one of its own (B.13, B.14).
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

  // §10.11: a viewer reads the chat and is offered nothing that would write.
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

  // §13.9: Settled replaced Archive, and the client offers no archive action,
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
