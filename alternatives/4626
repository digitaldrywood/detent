// The client half of decisions.md §10 ("Corrections after the first-slice
// review"), driven against the mock hub over real HTTP and a real event
// stream. Each test names the correction it covers.
import * as Effect from "effect/Effect";
import * as Option from "effect/Option";
import type { Result } from "effect/Result";
import { afterEach, describe, expect, it } from "vitest";

import type {
  ConversationListResponse,
  CreateConversationResponse,
  Receipt,
} from "../src/contracts/index.ts";
import {
  applyConversationEvent,
  isBackwardDelivery,
  questionLocked,
  type ConversationDetail,
  type PendingControl,
} from "../src/runtime/state/conversationState.ts";
import { reconnectDelayMs } from "../src/runtime/state/conversations.ts";
import { makeHarness, PROJECT, type Harness } from "./harness.ts";

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

async function open(options: Parameters<typeof makeHarness>[0] = {}): Promise<Harness> {
  harness = await makeHarness(options);
  return harness;
}

function conversationAtom(h: Harness, conversationId: string) {
  return h.client.conversations.stateAtom({
    environmentId: "hub",
    projectId: PROJECT,
    conversationId,
  });
}

function detailOf(value: { data: Option.Option<ConversationDetail> }): ConversationDetail {
  return Option.getOrThrow(value.data);
}

async function create(
  h: Harness,
  key: string,
  text: string,
  messageKey = `${key}_msg`,
): Promise<Result<typeof CreateConversationResponse.Type, { readonly code?: string }>> {
  return (await h.run(
    h.client.effects.createConversation({
      projectId: PROJECT,
      key,
      firstMessage: { key: messageKey, text },
    }),
  )) as Result<typeof CreateConversationResponse.Type, { readonly code?: string }>;
}

async function list(
  h: Harness,
  settled?: boolean,
): Promise<typeof ConversationListResponse.Type> {
  const page = await h.run(
    h.client.http
      .listOrganizationConversations({ limit: 100, ...(settled === undefined ? {} : { settled }) })
      .pipe(Effect.orDie),
  );
  return page;
}

describe("§10.2 creating a conversation is idempotent", () => {
  it("returns the stored response and creates one conversation when the key is reused", async () => {
    const h = await open();
    const first = await create(h, "cmd_create_once", "Why does the lock lapse?");
    const second = await create(h, "cmd_create_once", "Why does the lock lapse?");
    expect(first._tag).toBe("Success");
    expect(second._tag).toBe("Success");
    if (first._tag !== "Success" || second._tag !== "Success") return;
    expect(second.success.conversation.id).toBe(first.success.conversation.id);
    const page = await list(h);
    expect(page.conversations).toHaveLength(1);
  });

  it("refuses the same key with a different payload", async () => {
    const h = await open();
    await create(h, "cmd_create_conflict", "First intent");
    const other = await create(h, "cmd_create_conflict", "A different intent");
    expect(other._tag).toBe("Failure");
    if (other._tag !== "Failure") return;
    expect((other.failure as { code: string }).code).toBe("idempotency_conflict");
    const page = await list(h);
    expect(page.conversations).toHaveLength(1);
  });

  it("rejects a create with no key", async () => {
    const h = await open();
    const refused = await create(h, "", "No key here");
    expect(refused._tag).toBe("Failure");
    if (refused._tag !== "Failure") return;
    expect((refused.failure as { code: string }).code).toBe("invalid_request");
  });
});

describe("§10.3 retry re-queues one message", () => {
  it("re-queues a hub-persisted message whose delivery was lost", async () => {
    const h = await open({ coordinator: "runner" });
    const created = await create(h, "cmd_retry_create", "Fix the lease renewal");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    // A runner takes the chat, then loses its lease: the message it was handed
    // can no longer be established, so it is `unknown` and waits for the user.
    await h.control(`runner/${conversationId}/start`);
    await h.control(`runner/${conversationId}/lose`);
    const lost = await h.waitFor(
      atom,
      (value) =>
        detailOf(value).messages.some(
          (message) => message.role === "user" && message.delivery === "unknown",
        ),
      "the lost message",
    );
    const message = detailOf(lost).messages.find((candidate) => candidate.role === "user");
    expect(message).toBeDefined();
    if (message === undefined) return;

    const receipt = (await h.run(
      h.client.effects.retryMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_retry_1",
        messageId: message.id,
      }),
    )) as Receipt | null;
    // The receipt belongs to the retry key and reports `queued` (§10.3).
    expect(receipt?.status).toBe("queued");
    expect(receipt?.key).toBe("cmd_retry_1");
    expect(receipt?.message_id).toBe(message.id);

    const requeued = await h.waitFor(
      atom,
      (value) =>
        detailOf(value).messages.some(
          (candidate) => candidate.id === message.id && candidate.delivery === "queued",
        ),
      "the re-queued message",
    );
    // Nothing is duplicated: the same message id came back, not a second one.
    expect(
      detailOf(requeued).messages.filter((candidate) => candidate.role === "user"),
    ).toHaveLength(1);
  }, 20_000);

  it("refuses a retry on a message that is not retryable", async () => {
    const h = await open();
    const created = await create(h, "cmd_retry_bad", "Delivered already");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const snapshot = await h.run(
      h.client.http.getConversation({ projectId: PROJECT, conversationId }).pipe(Effect.orDie),
    );
    const message = snapshot.messages.find((candidate) => candidate.role === "user");
    expect(message?.delivery).toBe("delivered");
    const receipt = (await h.run(
      h.client.effects.retryMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_retry_2",
        messageId: message?.id ?? "msg_missing",
      }),
    )) as Receipt | null;
    // A refused command produces no receipt; the hub said `invalid_request`.
    expect(receipt).toBeNull();
  }, 20_000);

  it("re-sends an optimistic entry under its original key when no message exists yet", async () => {
    const h = await open();
    const created = await create(h, "cmd_outbox_create", "First");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    // The hub refuses the send outright, so no message was created and the
    // client holds an outbox entry with nothing to name.
    await h.control("queue-full");
    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_outbox_1",
        text: "Second",
        expected: { attempt_id: null, turn_id: null },
      }),
    );
    const failed = await h.waitFor(
      atom,
      (value) => detailOf(value).pending.some((entry) => entry.status === "failed"),
      "the failed outbox entry",
    );
    const entry = detailOf(failed).pending[0];
    expect(entry?.messageId).toBeNull();
    if (entry === undefined) return;

    await h.run(
      h.client.effects.retryPending({
        projectId: PROJECT,
        conversationId,
        key: "cmd_retry_3",
        entry,
      }),
    );
    const settled = await h.waitFor(
      atom,
      (value) =>
        detailOf(value).messages.some((message) => message.text === "Second") &&
        detailOf(value).pending.length === 0,
      "the accepted message",
    );
    // Exactly one message, under the original command key.
    const seconds = detailOf(settled).messages.filter((message) => message.text === "Second");
    expect(seconds).toHaveLength(1);
    expect(seconds[0]?.command_key).toBe("cmd_outbox_1");
  }, 20_000);
});

describe("§10.5 the conversation carries its message count", () => {
  it("counts the whole history, not the loaded page", async () => {
    const h = await open();
    const created = await create(h, "cmd_count_create", "One");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    const answered = await h.waitFor(
      atom,
      (value) =>
        Option.isSome(value.data) && detailOf(value).conversation.message_count >= 2,
      "the assistant reply",
    );
    expect(detailOf(answered).conversation.message_count).toBe(
      detailOf(answered).messages.length,
    );
  }, 20_000);
});

describe("§10.11 a read-only viewer", () => {
  it("reports no writable project and is refused if it writes anyway", async () => {
    const h = await open({ account: "read_only" });
    expect(h.client.bootstrap.projects.every((project) => !project.can_write)).toBe(true);
    const refused = await h.run(
      h.client.http
        .createConversation({ projectId: PROJECT, key: "cmd_ro", title: "Nope" })
        .pipe(Effect.result),
    );
    expect(refused._tag).toBe("Failure");
    if (refused._tag !== "Failure") return;
    expect(refused.failure).toMatchObject({ status: 403, code: "forbidden" });
  });
});

describe("§13.9 settled replaces archive", () => {
  it("has no archive endpoint left to call", async () => {
    const h = await open();
    const created = await create(h, "cmd_settle_create", "Settle me");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const response = await fetch(
      `${h.hub.url}${h.client.http.apiBase}/projects/${PROJECT}/conversations/${conversationId}/archive`,
      { method: "POST", headers: { "X-CSRF-Token": h.client.http.csrfToken } },
    );
    expect(response.status).toBe(404);
  }, 20_000);

  it("moves a settled conversation off the active shelf and back", async () => {
    const h = await open();
    const created = await create(h, "cmd_settle_list", "Settle me too");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;

    // The hub settles a conversation itself (§14); the mock exposes the same
    // transition so the shelf can be checked without waiting a day.
    await settle(h, conversationId, true);
    expect((await list(h, false)).conversations).toHaveLength(0);
    const shelved = (await list(h, true)).conversations;
    expect(shelved).toHaveLength(1);
    expect(shelved[0]?.status).toBe("settled");

    // Activity unsettles it.
    await settle(h, conversationId, false);
    expect((await list(h, false)).conversations).toHaveLength(1);
  }, 20_000);
});

/** The mock's stand-in for the hub's own settle window (§14). */
async function settle(h: Harness, conversationId: string, settled: boolean): Promise<void> {
  await h.control("settle", { conversation: conversationId, settled });
}

describe("the delivery ladder never runs backwards", () => {
  const control: PendingControl = {
    key: "cmd_answer_1",
    kind: "answer",
    questionId: "q_1",
    attemptId: "att_1",
    createdAt: "2026-09-09T10:00:00Z",
    status: "sent",
    error: null,
    errorCode: null,
    receiptStatus: "sent",
    expected: { attempt_id: "att_1", turn_id: "turn_1" },
    answers: { q: ["yes"] },
  };

  it("ranks the ladder and rejects a step back", () => {
    expect(isBackwardDelivery("sent", "queued")).toBe(true);
    expect(isBackwardDelivery("queued", "sent")).toBe(false);
    expect(isBackwardDelivery(null, "queued")).toBe(false);
    // Terminal outcomes are never behind anything.
    expect(isBackwardDelivery("sent", "unknown")).toBe(false);
  });

  it("keeps an answered question locked when a slow receipt reports queued", () => {
    const base = {
      conversation: {} as ConversationDetail["conversation"],
      messages: [],
      deltas: {},
      questions: [],
      receipts: {},
      pending: [],
      controls: [control],
      staleExecution: false,
      page: { hasMore: false, loadingOlder: false, oldestSeq: null },
    } as unknown as ConversationDetail;
    const next = applyConversationEvent(base, {
      type: "command.receipt",
      data: {
        key: "cmd_answer_1",
        kind: "answer",
        status: "queued",
        message_id: null,
        question_id: "q_1",
        error: null,
        updated_at: "2026-09-09T10:00:01Z",
      },
    });
    expect(next.controls[0]?.status).toBe("sent");
    expect(
      questionLocked(
        { status: "pending" } as never,
        next.controls[0],
      ),
    ).toBe(true);
  });
});

describe("reconnects are bounded", () => {
  it("backs off exponentially and stops at the cap", () => {
    expect(reconnectDelayMs(1)).toBe(1_000);
    expect(reconnectDelayMs(2)).toBe(2_000);
    expect(reconnectDelayMs(5)).toBe(16_000);
    expect(reconnectDelayMs(12)).toBe(30_000);
  });
});

describe("the event stream survives what it cannot read", () => {
  it("skips an undecodable frame instead of tearing the stream down", async () => {
    const h = await open();
    const created = await create(h, "cmd_bad_frame", "Keep the stream");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    // A frame this client's schema rejects. Before, it failed the stream and
    // the client reconnected every second from an unchanged cursor, re-reading
    // the same frame for ever.
    await h.control("bad-frame");
    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_after_bad_frame",
        text: "Still listening",
        expected: { attempt_id: null, turn_id: null },
      }),
    );
    const live = await h.waitFor(
      atom,
      (value) =>
        Option.isSome(value.data) &&
        detailOf(value).messages.some((message) => message.text === "Still listening"),
      "the message after the unreadable frame",
    );
    expect(live.status).toBe("live");
  }, 20_000);

  it("comes back after the hub closes the stream with server_error", async () => {
    const h = await open();
    const created = await create(h, "cmd_server_error", "Hold this line");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    await h.control("server-error");
    await h.control("drop-open-streams");
    // `server_error` says nothing about the conversation, so the transcript
    // stays and the client reconnects on its backoff.
    const recovered = await h.waitFor(
      atom,
      (value) => value.status === "live" && Option.isSome(value.data),
      "the reconnected stream",
    );
    expect(Option.isSome(recovered.closedReason)).toBe(false);
    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_after_server_error",
        text: "Back again",
        expected: { attempt_id: null, turn_id: null },
      }),
    );
    await h.waitFor(
      atom,
      (value) =>
        Option.isSome(value.data) &&
        detailOf(value).messages.some((message) => message.text === "Back again"),
      "the message after the reconnect",
    );
  }, 30_000);
});

describe("a slow command response never contradicts the stream", () => {
  it("does not re-add a bubble for a message the stream already delivered", async () => {
    // The POST is forwarded, so the hub accepts the message and the stream
    // delivers it; only then does this fetch answer with a failure. The client
    // must not turn an accepted message back into an unconfirmed bubble.
    let landed: (() => void) | undefined;
    const delivered = new Promise<void>((resolve) => {
      landed = resolve;
    });
    let deliveredSeen = false;
    const racingFetch: typeof globalThis.fetch = async (input, init) => {
      const target = String(input);
      const response = await fetch(input as string, init);
      if (!target.endsWith("/commands")) return response;
      await response.text();
      if (!deliveredSeen) {
        deliveredSeen = true;
        await delivered;
      }
      return new Response(JSON.stringify({ code: "invalid", message: "Late failure." }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      });
    };

    const h = await open({ fetch: racingFetch });
    const created = await create(h, "cmd_race_create", "First");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    const send = h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_race_1",
        text: "Raced",
        expected: { attempt_id: null, turn_id: null },
      }),
    );
    await h.waitFor(
      atom,
      (value) =>
        Option.isSome(value.data) &&
        detailOf(value).messages.some((message) => message.command_key === "cmd_race_1"),
      "the accepted message",
    );
    landed?.();
    await send;

    const settled = await h.waitFor(
      atom,
      (value) => Option.isSome(value.data),
      "the settled state",
    );
    expect(
      detailOf(settled).messages.filter((message) => message.command_key === "cmd_race_1"),
    ).toHaveLength(1);
    expect(detailOf(settled).pending).toHaveLength(0);
  }, 30_000);
});

describe("an outbox control carries what its retry needs", () => {
  it("retries after the view that sent it is gone", async () => {
    const h = await open();
    const created = await create(h, "cmd_control_retry", "Start something");
    if (created._tag !== "Success") throw new Error("create failed");
    const conversationId = created.success.conversation.id;
    const atom = conversationAtom(h, conversationId);
    h.mount(atom);
    await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

    await h.control("queue-full");
    await h.run(
      h.client.effects.sendInterrupt({
        projectId: PROJECT,
        conversationId,
        key: "cmd_interrupt_1",
        expected: { attempt_id: null, turn_id: null },
      }),
    );
    const rejected = await h.waitFor(
      atom,
      (value) =>
        Option.isSome(value.data) &&
        detailOf(value).controls.some((entry) => entry.status === "rejected"),
      "the rejected control",
    );
    const entry = detailOf(rejected).controls[0];
    expect(entry?.expected).toEqual({ attempt_id: null, turn_id: null });
    if (entry === undefined) return;

    // Nothing but the entry is needed: the payload lives on it, so a reader
    // who navigated away and back can still press Retry.
    await h.run(h.client.effects.retryControl({ projectId: PROJECT, conversationId, entry }));
    const settled = await h.waitFor(
      atom,
      (value) =>
        Option.isSome(value.data) &&
        detailOf(value).controls.some((candidate) => candidate.status === "sent"),
      "the resent control",
    );
    expect(detailOf(settled).controls).toHaveLength(1);
    expect(detailOf(settled).controls[0]?.key).toBe("cmd_interrupt_1");
  }, 20_000);
});
