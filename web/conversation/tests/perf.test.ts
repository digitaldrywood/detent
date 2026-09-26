// Performance budgets for the client runtime (plan task O03).
//
// These are budgets, not benchmarks: they exist to catch an accidental
// quadratic or a subscription leak, not to track milliseconds. The numbers are
// deliberately generous — orders of magnitude above what the work costs on a
// developer machine — so a slow shared CI runner does not fail the suite while
// real regression still does.
//
// No DOM is involved. The history is built through the mock hub over real
// HTTP, and what is measured afterwards is the pure decode-and-merge path the
// runtime uses: schema decode, `detailFromSnapshot`, `mergeOlderMessages` and
// `applyConversationEvent`.
import * as Schema from "effect/Schema";
import { afterEach, describe, expect, it } from "vitest";

import type { Result } from "effect/Result";

import { makeHarness, PROJECT, type Harness } from "./harness.ts";
import {
  ConversationSnapshot,
  MessagePageResponse,
  type CreateConversationResponse,
} from "../src/contracts/index.ts";
import {
  applyConversationEvent,
  deltaText,
  detailFromSnapshot,
  mergeOlderMessages,
  type ConversationDetail,
} from "../src/runtime/state/conversationState.ts";
import { conversationKey } from "../src/runtime/state/conversations.ts";

/** Messages the loaded transcript must hold before the budget is measured. */
const HISTORY_MESSAGES = 2_000;
/** Deltas in the streamed burst. A long turn is a few hundred chunks. */
const BURST_DELTAS = 500;

// Budgets, in milliseconds, for CI-class hardware. Measured on an M-series
// laptop the decode and merge of a 2,000-message history takes about 7ms and
// the 500-delta burst under 1ms, so both budgets leave roughly two orders of
// magnitude of headroom for a slower, contended runner. They are set to catch
// an accidental quadratic, not to police a few milliseconds.
const HISTORY_BUDGET_MS = 500;
const BURST_BUDGET_MS = 200;

const decodeSnapshot = Schema.decodeUnknownSync(ConversationSnapshot);
const decodePage = Schema.decodeUnknownSync(MessagePageResponse);

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

async function open(): Promise<Harness> {
  harness = await makeHarness();
  return harness;
}

async function createConversation(h: Harness, text: string, key: string) {
  const created = (await h.run(
    h.client.effects.createConversation({
      projectId: PROJECT,
      key: `${key}_create`,
      firstMessage: { key, text },
    }),
  )) as Result<typeof CreateConversationResponse.Type, unknown>;
  if (created._tag !== "Success") throw new Error("Creating the conversation failed.");
  return created.success.conversation.id;
}

function base(h: Harness): string {
  return `${h.hub.url}/api/v2/organizations/org_mock/projects/${PROJECT}/conversations`;
}

/**
 * Grows the conversation to at least `HISTORY_MESSAGES` messages through the
 * hub's own command endpoint. Every message command also runs the scripted
 * coordinator turn, so each one adds a user message and an assistant reply.
 */
async function growHistory(h: Harness, conversationId: string): Promise<void> {
  const commands = HISTORY_MESSAGES / 2 - 1;
  const batch = 25;
  for (let sent = 0; sent < commands; sent += batch) {
    const chunk = Array.from({ length: Math.min(batch, commands - sent) }, (_, index) =>
      fetch(`${base(h)}/${conversationId}/commands`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          key: `perf_${sent + index}`,
          kind: "message",
          text: `History message ${sent + index}: why does the lease lapse under load?`,
        }),
      }).then((response) => response.text()),
    );
    await Promise.all(chunk);
  }
}

describe("client performance budgets", () => {
  it(
    "decodes and merges a 2,000-message history and a 500-delta burst inside budget",
    { timeout: 120_000 },
    async () => {
      const h = await open();
      const conversationId = await createConversation(h, "Seed the history", "perf_seed");
      await growHistory(h, conversationId);

      // The wire payloads, fetched before the clock starts: the budget covers
      // decode and merge, not the network.
      const rawSnapshot: unknown = await fetch(`${base(h)}/${conversationId}`).then((response) =>
        response.json(),
      );
      const rawOlder: unknown = await fetch(
        `${base(h)}/${conversationId}/messages?before=1000000&limit=${HISTORY_MESSAGES + 100}`,
      ).then((response) => response.json());

      const startedAt = performance.now();
      const snapshot = decodeSnapshot(rawSnapshot);
      let detail: ConversationDetail = detailFromSnapshot(snapshot);
      const older = decodePage(rawOlder);
      detail = mergeOlderMessages(detail, older.messages, older.next_cursor !== null);
      const historyMs = performance.now() - startedAt;

      expect(detail.messages.length).toBeGreaterThanOrEqual(HISTORY_MESSAGES);
      // Ordering survives the merge: the reducer is what the transcript reads.
      expect(detail.messages[0]?.seq).toBe(1);
      expect(detail.messages.at(-1)?.seq).toBe(detail.messages.length);

      const burstMessageId = detail.messages.at(-1)?.id ?? "msg_missing";
      const burstStartedAt = performance.now();
      for (let seq = 1; seq <= BURST_DELTAS; seq += 1) {
        detail = applyConversationEvent(detail, {
          type: "message.delta",
          data: { message_id: burstMessageId, seq, text: `chunk ${seq} ` },
        });
      }
      const assembled = deltaText(detail.deltas[burstMessageId]);
      const burstMs = performance.now() - burstStartedAt;

      // Every delta is in the buffer once, in seq order.
      expect(assembled.startsWith("chunk 1 chunk 2 ")).toBe(true);
      expect(assembled.endsWith(`chunk ${BURST_DELTAS} `)).toBe(true);
      expect(Object.keys(detail.deltas[burstMessageId]?.parts ?? {}).length).toBe(BURST_DELTAS);

      // Reported so a regression run says what it measured, not only that it
      // failed. `npx vitest run` prints this line.
      console.log(
        `perf: ${detail.messages.length} messages decoded and merged in ${historyMs.toFixed(1)}ms ` +
          `(budget ${HISTORY_BUDGET_MS}ms); ${BURST_DELTAS} deltas assembled in ${burstMs.toFixed(1)}ms ` +
          `(budget ${BURST_BUDGET_MS}ms)`,
      );

      expect(historyMs).toBeLessThan(HISTORY_BUDGET_MS);
      expect(burstMs).toBeLessThan(BURST_BUDGET_MS);
    },
  );

  it("holds one detail subscription while navigating between two conversations", async () => {
    const h = await open();
    const first = await createConversation(h, "First conversation", "perf_nav_first");
    const second = await createConversation(h, "Second conversation", "perf_nav_second");
    const keys = [first, second].map((id) =>
      conversationKey({ environmentId: "hub", projectId: PROJECT, conversationId: id }),
    );

    // One handle is registered for the lifetime of one open subscription
    // (`ConversationHandles.register`), so counting them counts subscriptions.
    const openKeys = () => keys.filter((key) => h.client.handles.get(key) !== undefined);
    const settle = async (predicate: () => boolean, label: string) => {
      const deadline = Date.now() + 10_000;
      while (!predicate()) {
        if (Date.now() > deadline) throw new Error(`Timed out waiting for ${label}.`);
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    };

    let unmount: (() => void) | undefined;
    // Three round trips: open, leave, open the other, six navigations in all.
    for (let step = 0; step < 6; step += 1) {
      const conversationId = step % 2 === 0 ? first : second;
      const key = keys[step % 2] as string;
      unmount?.();
      await settle(() => openKeys().length === 0, "the previous subscription to close");
      unmount = h.registry.mount(
        h.client.conversations.stateAtom({
          environmentId: "hub",
          projectId: PROJECT,
          conversationId,
        }),
      );
      await settle(() => h.client.handles.get(key) !== undefined, "the subscription to open");
      // The one just opened is the only one: nothing accumulates.
      expect(openKeys()).toEqual([key]);
    }

    unmount?.();
    await settle(() => openKeys().length === 0, "the last subscription to close");
  });
});
