// @vitest-environment jsdom
import { RegistryProvider } from "@effect/atom-react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it } from "vitest";

import { startMockHub, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { makeRouter } from "../../src/app/router.tsx";
import { setComposerText } from "./composerInput.ts";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";
import { ATTACHMENT_MAX_BYTES, ATTACHMENT_MAX_PER_MESSAGE } from "../../src/contracts/index.ts";

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

/** See `app.test.tsx`: jsdom's `AbortSignal` is from another realm to undici's. */
const sameRealmFetch: typeof globalThis.fetch = (input, init) => {
  const { signal: _abort, ...rest } = (init ?? {}) as RequestInit;
  return globalThis.fetch(input as string, rest);
};

async function mountApp() {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub" });
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({
    origin: hub.url,
    bootstrap,
    transport: fetchEventStreamTransport(sameRealmFetch),
    heartbeatTimeoutMs: 20_000,
    fetch: sameRealmFetch,
  });
  const router = makeRouter(createMemoryHistory({ initialEntries: ["/chat"] }));
  render(
    <RegistryProvider>
      <ClientContext.Provider value={client}>
        <RouterProvider router={router} />
      </ClientContext.Provider>
    </RegistryProvider>,
  );
  return { router };
}

async function toastViewport(): Promise<HTMLElement> {
  return await waitFor(
    () => {
      const viewport = document.querySelector<HTMLElement>('[data-slot="toast-viewport"]');
      if (viewport === null) throw new Error("no toast viewport");
      return viewport;
    },
    { timeout: 5_000 },
  );
}

/** Opens a conversation, which is what an attachment is staged against. */
async function openConversation(router: ReturnType<typeof makeRouter>): Promise<void> {
  const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
    timeout: 5_000,
  });
  await setComposerText(composer, "First message");
  fireEvent.keyDown(composer, { key: "Enter" });
  await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
    timeout: 5_000,
  });
}

function textFile(name: string, size = 12, type = "text/plain"): File {
  return new File(["x".repeat(size)], name, { type });
}

/** The shape `makeWorkspaceFileDropHandlers` reads off a drag event. */
function transfer(files: readonly File[]) {
  return { types: ["Files"], files, dropEffect: "none" };
}

function dropTarget(): HTMLElement {
  const target = document.querySelector<HTMLElement>("[data-chat-workspace-drop-target]");
  if (target === null) throw new Error("The chat surface is not a drop target.");
  return target;
}

describe("the chat dropzone", () => {
  it("shows the overlay on a file drag and queues the dropped files as chips", async () => {
    const { router } = await mountApp();
    await openConversation(router);

    const target = dropTarget();
    fireEvent.dragEnter(target, { dataTransfer: transfer([textFile("notes.md")]) });
    const overlay = document.querySelector("[data-chat-workspace-drop-overlay]");
    expect(overlay).not.toBeNull();
    expect(overlay?.textContent).toContain("Drop files to attach");

    fireEvent.drop(target, { dataTransfer: transfer([textFile("notes.md")]) });

    await waitFor(() =>
      expect(document.querySelector("[data-chat-workspace-drop-overlay]")).toBeNull(),
    );
    const chip = await screen.findByTestId("composer-attachment", undefined, { timeout: 5_000 });
    expect(chip.textContent).toContain("notes.md");
    // The upload lands and the chip stops saying so.
    await waitFor(() => expect(chip.textContent).not.toContain("Uploading…"), { timeout: 5_000 });
  });

  it("refuses more than the per-message limit with T3's sentence", async () => {
    const { router } = await mountApp();
    await openConversation(router);

    const many = Array.from({ length: ATTACHMENT_MAX_PER_MESSAGE + 2 }, (_unused, index) =>
      textFile(`file-${index}.txt`),
    );
    fireEvent.drop(dropTarget(), { dataTransfer: transfer(many) });

    const toasts = await toastViewport();
    await waitFor(
      () =>
        expect(toasts.textContent).toContain(
          `You can attach up to ${ATTACHMENT_MAX_PER_MESSAGE} files per message.`,
        ),
      { timeout: 5_000 },
    );
    await waitFor(() =>
      expect(screen.getAllByTestId("composer-attachment")).toHaveLength(
        ATTACHMENT_MAX_PER_MESSAGE,
      ),
    );
  });

  it("refuses a file over the size limit with T3's sentence", async () => {
    const { router } = await mountApp();
    await openConversation(router);

    // Reported size only: the bytes never leave the client, because the
    // refusal happens before the upload.
    const huge = textFile("huge.txt", 8);
    Object.defineProperty(huge, "size", { value: ATTACHMENT_MAX_BYTES + 1 });
    fireEvent.drop(dropTarget(), { dataTransfer: transfer([huge]) });

    const toasts = await toastViewport();
    await waitFor(
      () => expect(toasts.textContent).toContain("'huge.txt' exceeds the 20 MB attachment limit."),
      { timeout: 5_000 },
    );
    expect(screen.queryAllByTestId("composer-attachment")).toHaveLength(0);
  });

  it("keeps the draft and offers a retry when the upload fails", async () => {
    const { router } = await mountApp();
    await openConversation(router);

    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    await setComposerText(composer, "Here is the log");
    await fetch(`${hub!.url}/__mock/fail-upload`, { method: "POST" });

    fireEvent.drop(dropTarget(), { dataTransfer: transfer([textFile("run.log")]) });

    const chip = await screen.findByTestId("composer-attachment", undefined, { timeout: 5_000 });
    await waitFor(
      () =>
        expect(
          within(chip).getByRole("button", { name: "Retry upload for run.log" }),
        ).not.toBeNull(),
      { timeout: 5_000 },
    );
    // The draft is untouched: a failed upload loses the file, never the words.
    expect(composer.textContent).toContain("Here is the log");

    // Retrying succeeds, because the hub only refuses once.
    fireEvent.click(within(chip).getByRole("button", { name: "Retry upload for run.log" }));
    await waitFor(
      () =>
        expect(within(chip).queryByRole("button", { name: "Retry upload for run.log" })).toBeNull(),
      { timeout: 5_000 },
    );
  });

  it("sends the attachment ids with the message and shows them on the turn", { timeout: 20_000 }, async () => {
    const { router } = await mountApp();
    await openConversation(router);
    const conversationId = router.state.location.pathname.replace("/chat/c/", "");

    fireEvent.drop(dropTarget(), { dataTransfer: transfer([textFile("plan.md")]) });
    const chip = await screen.findByTestId("composer-attachment", undefined, { timeout: 5_000 });
    await waitFor(
      () => {
        expect(chip.textContent).not.toContain("Uploading…");
        expect(within(chip).queryByRole("button", { name: /^Retry upload/ })).toBeNull();
      },
      { timeout: 5_000 },
    );

    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    await setComposerText(composer, "Read the plan");
    fireEvent.keyDown(composer, { key: "Enter" });

    // The chip is released once the hub accepts the message.
    await waitFor(() => expect(screen.queryAllByTestId("composer-attachment")).toHaveLength(0), {
      timeout: 5_000,
    });

    // The hub bound the attachment to the message the command named, which is
    // what proves the ids travelled in the `message` command.
    await waitFor(
      async () => {
        const response = await fetch(
          `${hub!.url}${client!.http.apiBase}/projects/proj_alpha/conversations/${conversationId}`,
        );
        const snapshot = (await response.json()) as {
          messages: Array<{ text: string; attachments?: Array<{ name: string }> }>;
        };
        const sent = snapshot.messages.find((message) => message.text === "Read the plan");
        expect(sent?.attachments?.[0]?.name).toBe("plan.md");
      },
      { timeout: 5_000 },
    );

    const link = await screen.findByRole("link", { name: /plan\.md/ }, { timeout: 5_000 });
    expect(link.getAttribute("download")).toBe("plan.md");
    expect(link.getAttribute("href")).toBeTruthy();
  });

  it("takes a drop on the new-chat page, uploads after the create and names the files on the first message", { timeout: 20_000 }, async () => {
    const { router } = await mountApp();
    // The draft page: no conversation yet, so the file waits as a staged chip.
    expect(router.state.location.pathname).toBe("/chat");
    await screen.findByLabelText("Message", undefined, { timeout: 5_000 });
    const target = dropTarget();
    fireEvent.dragEnter(target, { dataTransfer: transfer([textFile("brief.md")]) });
    expect(document.querySelector("[data-chat-workspace-drop-overlay]")).not.toBeNull();
    fireEvent.drop(target, { dataTransfer: transfer([textFile("brief.md")]) });
    const chip = await screen.findByTestId("composer-attachment", undefined, { timeout: 5_000 });
    expect(chip.textContent).toContain("brief.md");
    expect(chip.textContent).not.toContain("Uploading…");

    const composer = await screen.findByLabelText<HTMLElement>("Message", undefined, {
      timeout: 5_000,
    });
    await setComposerText(composer, "Start from the brief");
    fireEvent.keyDown(composer, { key: "Enter" });

    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/chat\/c\/conv_/), {
      timeout: 10_000,
    });
    const conversationId = router.state.location.pathname.replace("/chat/c/", "");
    await waitFor(
      async () => {
        const response = await fetch(
          `${hub!.url}${client!.http.apiBase}/projects/proj_alpha/conversations/${conversationId}`,
        );
        const snapshot = (await response.json()) as {
          messages: Array<{ text: string; attachments?: Array<{ name: string }> }>;
        };
        const sent = snapshot.messages.find((message) => message.text === "Start from the brief");
        expect(sent?.attachments?.[0]?.name).toBe("brief.md");
      },
      { timeout: 5_000 },
    );
  });

  it("opens a file picker from the paperclip on the draft and on the conversation", async () => {
    const { router } = await mountApp();
    // The draft page's paperclip is live too: a picked file waits as a staged chip.
    const before = await screen.findByTestId("composer-attach", undefined, { timeout: 5_000 });
    expect(before.getAttribute("disabled")).toBeNull();

    await openConversation(router);
    await waitFor(() =>
      expect(screen.getByTestId("composer-attach").getAttribute("disabled")).toBeNull(),
    );

    const input = screen.getByTestId<HTMLInputElement>("composer-attach-input");
    expect(input.multiple).toBe(true);
    fireEvent.change(input, { target: { files: [textFile("picked.txt")] } });
    const chip = await screen.findByTestId("composer-attachment", undefined, { timeout: 5_000 });
    expect(chip.textContent).toContain("picked.txt");
  });
});
