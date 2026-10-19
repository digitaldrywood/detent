// Runtime integration tests. These drive the client runtime against the mock
// hub over real HTTP and a real event stream; no DOM is involved.
import * as Option from "effect/Option";
import { AsyncResult } from "effect/unstable/reactivity";
import { afterEach, describe, expect, it } from "vitest";

import type { Result } from "effect/Result";

import { makeHarness, PROJECT, type Harness } from "./harness.ts";
import { deltaText } from "../src/runtime/state/conversationState.ts";
import type { CreateConversationResponse } from "../src/contracts/index.ts";

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

async function open(): Promise<Harness> {
  harness = await makeHarness();
  return harness;
}

function conversationAtom(h: Harness, conversationId: string) {
  return h.client.conversations.stateAtom({
    environmentId: "hub",
    projectId: PROJECT,
    conversationId,
  });
}

async function createWithFirstMessage(h: Harness, text: string, key = `cmd_${text.length}_a`) {
  const created = (await h.run(
    h.client.effects.createConversation({
      projectId: PROJECT,
      key: `${key}_create`,
      firstMessage: { key, text },
    }),
  )) as Result<typeof CreateConversationResponse.Type, unknown>;
  if (created._tag !== "Success") throw new Error("Creating the conversation failed.");
  return created.success;
}

describe("conversation runtime", () => {
  it("creates a conversation with a first message and streams the reply", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Why does the lock lapse?");
    expect(created.receipt?.status).toBe("delivered");

    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.messages.some((message) => message.role === "assistant" && message.text.length > 0),
        }),
      "the assistant reply",
    );
    const detail = Option.getOrThrow(state.data);
    expect(detail.messages[0]?.text).toBe("Why does the lock lapse?");
    expect(detail.messages.at(-1)?.role).toBe("assistant");
    expect(detail.messages.at(-1)?.text).toContain("renewal");
  });

  it("assembles deltas in seq order and stays stable when the window replays", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Stream me a reply");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);

    const settled = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.messages.some((message) => message.role === "assistant" && message.text.length > 0),
        }),
      "the streamed reply",
    );
    const first = Option.getOrThrow(settled.data);
    const assistantText = first.messages.at(-1)?.text ?? "";

    // A second subscription replays the same window from seq 0.
    const replayAtom = h.client.conversations.stateAtom({
      environmentId: "hub",
      projectId: PROJECT,
      conversationId: created.conversation.id,
    });
    expect(replayAtom).toBe(atom);

    // Deltas are retired by the final `message.updated`, so the assembled text
    // is the concatenation of the parts exactly once.
    expect(Object.keys(first.deltas)).toHaveLength(0);
    expect(assistantText).toBe(
      "Looking at the lock renewal path now. The renewal returns before the handoff completes, " +
        "so the lease can lapse under load. I would move the renewal behind the handoff acknowledgement.",
    );
  });

  it("keeps optimistic messages out of the transcript twice", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "First");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId: created.conversation.id,
        key: "cmd_optimistic_1",
        text: "Second",
      }),
    );

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.messages.some((message) => message.text === "Second"),
        }),
      "the accepted second message",
    );
    const detail = Option.getOrThrow(state.data);
    expect(detail.messages.filter((message) => message.text === "Second")).toHaveLength(1);
    expect(detail.pending.filter((entry) => entry.key === "cmd_optimistic_1")).toHaveLength(0);
  });

  it("leaves an unknown outcome visible and never retries it", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Base");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    await h.control("unknown-outcome");
    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId: created.conversation.id,
        key: "cmd_unknown_1",
        text: "Did this land?",
      }),
    );

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.pending.some(
              (entry) => entry.key === "cmd_unknown_1" && entry.status === "unknown",
            ),
        }),
      "the unknown pending entry",
    );
    const detail = Option.getOrThrow(state.data);
    expect(detail.messages.some((message) => message.text === "Did this land?")).toBe(false);

    // Nothing resends it: after a settle window the entry is still unknown and
    // no message with that key has appeared.
    await new Promise((resolve) => setTimeout(resolve, 300));
    const later = Option.getOrThrow(h.read(atom)!.data);
    expect(later.pending.find((entry) => entry.key === "cmd_unknown_1")?.status).toBe("unknown");
    expect(later.messages.some((message) => message.command_key === "cmd_unknown_1")).toBe(false);
  });

  it("surfaces queue_full as a retryable failure without losing the text", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Base");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    await h.control("queue-full");
    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId: created.conversation.id,
        key: "cmd_queue_1",
        text: "Please queue me",
      }),
    );

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.pending.some((entry) => entry.key === "cmd_queue_1"),
        }),
      "the rejected pending entry",
    );
    const entry = Option.getOrThrow(state.data).pending.find(
      (candidate) => candidate.key === "cmd_queue_1",
    );
    expect(entry?.retryable).toBe(true);
    expect(entry?.text).toBe("Please queue me");
  });

  it("re-snapshots when the hub reports an expired cursor", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Expire me");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    const live = await h.waitFor(
      atom,
      (value) => value.status === "live" && Option.isSome(value.data),
      "the live stream",
    );
    const before = Option.getOrThrow(live.data).messages.length;

    await h.control("expire-cursors", { below: 1_000_000 });
    await h.control("drop-open-streams");

    const recovered = await h.waitFor(
      atom,
      (value) =>
        value.status === "live" &&
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.messages.length >= before,
        }),
      "the re-snapshot after cursor_expired",
    );
    expect(Option.getOrThrow(recovered.data).messages.length).toBe(before);
    expect(Option.isNone(recovered.closedReason)).toBe(true);
  });

  it("keeps the transcript on screen while it re-snapshots an expired cursor", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Keep me visible");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(
      atom,
      (value) => value.status === "live" && Option.isSome(value.data),
      "the live stream",
    );

    // Every emission from here on is watched: losing the transcript, even for
    // one frame, unmounts the thread and takes the scroll position and the
    // composer draft with it (U05).
    let emptied = false;
    const unsubscribe = h.registry.subscribe(atom, (value) => {
      const detail = Option.getOrUndefined(AsyncResult.value(value));
      if (detail !== undefined && Option.isNone(detail.data)) emptied = true;
    });

    await h.control("expire-cursors", { below: 1_000_000 });
    await h.control("drop-open-streams");
    await h.waitFor(
      atom,
      (value) => value.status === "live" && Option.isSome(value.data),
      "the re-snapshot",
    );
    unsubscribe();

    expect(emptied).toBe(false);
    const detail = Option.getOrThrow(h.read(atom)!.data);
    expect(detail.messages.some((message) => message.text === "Keep me visible")).toBe(true);
  });

  it("leaves the conversation when access is revoked", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "Revoke me");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    await h.control("revoke-access");
    await h.control("drop-open-streams");

    const gone = await h.waitFor(
      atom,
      (value) => Option.getOrUndefined(value.closedReason) === "access_revoked",
      "the access_revoked close",
    );
    expect(gone.status).toBe("gone");
    expect(Option.isNone(gone.data)).toBe(true);
  });

  it("refreshes the conversation list from an open conversation's updates", async () => {
    const h = await open();
    const listAtom = h.client.list.stateAtom("hub");
    h.mount(listAtom);
    await h.waitFor(listAtom, (value) => value.status === "live", "the initial list");

    const created = await createWithFirstMessage(h, "Show me in the sidebar");
    await h.run(h.client.effects.refreshList());

    const listed = await h.waitFor(
      listAtom,
      (value) => value.conversations.some((conversation) => conversation.id === created.conversation.id),
      "the new conversation in the list",
    );
    expect(listed.conversations[0]?.id).toBe(created.conversation.id);

    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");
    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId: created.conversation.id,
        key: "cmd_list_refresh",
        text: "Bump the activity",
      }),
    );
    const updated = await h.waitFor(
      listAtom,
      (value) =>
        value.conversations.some(
          (conversation) =>
            conversation.id === created.conversation.id && conversation.last_message_at !== null,
        ),
      "the list update from the open stream",
    );
    expect(
      updated.conversations.find((conversation) => conversation.id === created.conversation.id)
        ?.last_message_at,
    ).not.toBeNull();
  });

  it("pages older history without reordering the loaded transcript", async () => {
    const h = await open();
    const created = await createWithFirstMessage(h, "One");
    const atom = conversationAtom(h, created.conversation.id);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");
    for (const [index, text] of ["Two", "Three"].entries()) {
      await h.run(
        h.client.effects.sendMessage({
          projectId: PROJECT,
          conversationId: created.conversation.id,
          key: `cmd_page_${index}`,
          text,
        }),
      );
      await h.waitFor(
        atom,
        (value) =>
          Option.match(value.data, {
            onNone: () => false,
            onSome: (detail) => detail.messages.some((message) => message.text === text),
          }),
        `the message ${text}`,
      );
    }
    await h.run(
      h.client.effects.loadOlder({ projectId: PROJECT, conversationId: created.conversation.id }),
    );
    const state = h.read(atom);
    const detail = Option.getOrThrow(state!.data);
    const seqs = detail.messages.map((message) => message.seq);
    expect(seqs).toEqual([...seqs].toSorted((left, right) => left - right));
    expect(new Set(detail.messages.map((message) => message.id)).size).toBe(detail.messages.length);
  });
});

describe("delta assembly", () => {
  it("joins parts by seq and is idempotent on replay", () => {
    const buffer = { parts: { 2: "world", 1: "hello " } };
    expect(deltaText(buffer)).toBe("hello world");
    expect(deltaText({ parts: { ...buffer.parts, 1: "hello " } })).toBe("hello world");
  });
});
